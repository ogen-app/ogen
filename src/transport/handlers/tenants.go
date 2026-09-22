package handlers

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// ProfileLifecycleEnqueuer enqueues the Zernio profile lifecycle jobs in the
// caller's transaction, so each commits atomically with the workspace it tracks:
// provisioning at create (CON-102 §6 FR2) and teardown at delete (CON-203). Both
// are local DB inserts — the Zernio calls happen later in the workers — so the
// request never blocks on Zernio reachability. Implemented by *queues.Enqueuer;
// kept as a narrow interface here so the handler doesn't import the jobs package
// and stays unit-testable.
type ProfileLifecycleEnqueuer interface {
	EnqueueBootstrapProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error
	EnqueueTeardownProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error
}

// EmailEnqueuer enqueues the CON-154 lifecycle emails (welcome + onboarding
// drip) in the signup transaction, so they commit atomically with the new
// tenant/user. Implemented by *queues.Enqueuer; a narrow interface here keeps
// the handler out of the jobs package and unit-testable.
type EmailEnqueuer interface {
	EnqueueWelcomeEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
	EnqueueDripTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
}

// Signup throttle budget (CON-162). POST /api/tenants is open and unauthenticated,
// so it is throttled per client IP to blunt automated mass account creation.
// Unlike login, every attempt is charged (a successful signup is exactly the
// abuse being limited), and there is no per-address dimension — each signup uses
// a fresh address, so the IP is the only meaningful key.
const (
	signupPerIPBurst = 10
	signupRateWindow = time.Hour
)

// signupThrottledMsg is the 429 body for a throttled signup; the signup form
// surfaces it verbatim.
const signupThrottledMsg = "Too many signup attempts. Please wait a minute and try again."

// TenantsHandler owns tenant provisioning (public self-service signup) and the
// tenant CRU surface (no delete). Tenants are the isolation boundary, so the
// read/update endpoints only ever operate on the caller's own tenant (CON-97).
type TenantsHandler struct {
	signup       *signup.Service
	tenantRepo   repository.TenantRepository
	cookieName   string
	secureCookie bool
	auth         fiber.Handler
	// ipLimiter throttles signup per client IP (CON-162).
	ipLimiter *keyedRateLimiter
	// activity records CON-125 authentication events (signup, tenant_updated).
	// Signup runs outside tenant scope, so it builds an explicit context. nil is
	// a no-op. Wired via SetActivityRecorder.
	activity *activity.Recorder
}

func NewTenantsHandler(signupSvc *signup.Service, tenantRepo repository.TenantRepository, cookieName string, secureCookie bool, auth fiber.Handler) *TenantsHandler {
	return &TenantsHandler{
		signup:       signupSvc,
		tenantRepo:   tenantRepo,
		cookieName:   cookieName,
		secureCookie: secureCookie,
		auth:         auth,
		ipLimiter:    newKeyedRateLimiter(signupPerIPBurst, signupRateWindow),
	}
}

// SetActivityRecorder wires the CON-125 activity recorder (nil-safe no-op).
func (h *TenantsHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

func (h *TenantsHandler) Register(app *fiber.App) {
	app.Post("/api/tenants", h.Signup) // public self-service signup

	g := app.Group("/api/tenants", h.auth)
	g.Get("/current", h.Current)
	g.Get("/:id", h.Get)
	g.Put("/:id", h.Update)
}

type signupRequest struct {
	Tenant struct {
		Name string `json:"name" validate:"required"`
	} `json:"tenant"`
	User struct {
		Name     string `json:"name"     validate:"required"`
		Email    string `json:"email"    validate:"required,email"`
		Password string `json:"password" validate:"required,min=8"`
	} `json:"user"`
}

type signupResponse struct {
	Tenant  *models.Tenant  `json:"tenant"`
	User    *models.User    `json:"user"`
	Session *models.Session `json:"session"`
}

// Signup godoc
// @Summary      Sign up (create tenant)
// @Description  Public self-service signup: atomically creates a tenant and its first user, opens a session, and returns the session cookie (CON-97 §7.1).
// @Tags         tenants
// @Accept       json
// @Produce      json
// @Param        body  body      signupRequest  true  "Signup payload"
// @Success      201   {object}  signupResponse
// @Failure      400   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/tenants [post]
func (h *TenantsHandler) Signup(c *fiber.Ctx) error {
	var req signupRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	// Throttle mass signup per client IP (CON-162). Every well-formed attempt is
	// charged — a successful signup is the abuse being limited — so a spike from
	// one source answers 429 with a Retry-After once the budget is spent.
	if ok, retry := h.ipLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, signupThrottledMsg)
	}

	// The transactional create + job enqueues live in the signup use case; the
	// handler keeps only transport: throttle, cookie, activity, response.
	res, err := h.signup.Create(reqCtx(c), signup.Input{
		TenantName: req.Tenant.Name,
		UserName:   req.User.Name,
		Email:      req.User.Email,
		Password:   req.User.Password,
	})
	if err != nil {
		if errors.Is(err, signup.ErrEmailInUse) {
			return fiber.NewError(fiber.StatusConflict, "email already in use")
		}
		return err
	}

	c.Cookie(&fiber.Cookie{
		Name:     h.cookieName,
		Value:    res.Session.ID,
		Path:     "/",
		Expires:  res.Session.ExpiresAt,
		HTTPOnly: true,
		Secure:   h.secureCookie,
		SameSite: "Lax",
	})

	h.activity.Record(
		logging.WithUserID(tenantctx.With(reqCtx(c), res.Tenant.ID), res.User.ID),
		activity.CategoryAuthentication, "signup",
		activity.WithEntity("tenant", res.Tenant.ID), activity.WithSource(activity.SourceAPI),
	)

	return c.Status(fiber.StatusCreated).JSON(signupResponse{Tenant: res.Tenant, User: res.User, Session: res.Session})
}

// Current godoc
// @Summary      Get current tenant
// @Description  Returns the authenticated caller's tenant.
// @Tags         tenants
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  models.Tenant
// @Failure      401  {object}  map[string]string
// @Router       /api/tenants/current [get]
func (h *TenantsHandler) Current(c *fiber.Ctx) error {
	return h.respondTenant(c, h.callerTenantID(c))
}

// Get godoc
// @Summary      Get tenant
// @Description  Returns a tenant by id. Only the caller's own tenant is visible; any other id returns 404 (no existence leak — CON-97 §12.3).
// @Tags         tenants
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Tenant id"
// @Success      200  {object}  models.Tenant
// @Failure      404  {object}  map[string]string
// @Router       /api/tenants/{id} [get]
func (h *TenantsHandler) Get(c *fiber.Ctx) error {
	if c.Params("id") != h.callerTenantID(c) {
		return fiber.NewError(fiber.StatusNotFound, "tenant not found")
	}
	return h.respondTenant(c, c.Params("id"))
}

type updateTenantRequest struct {
	Name string `json:"name" validate:"required"`
}

// Update godoc
// @Summary      Update tenant
// @Description  Updates the caller's own tenant name. Any other id returns 404. The slug is stable across renames (CON-97 §7.3).
// @Tags         tenants
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string               true  "Tenant id"
// @Param        body  body      updateTenantRequest  true  "Tenant payload"
// @Success      200   {object}  models.Tenant
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/tenants/{id} [put]
func (h *TenantsHandler) Update(c *fiber.Ctx) error {
	if c.Params("id") != h.callerTenantID(c) {
		return fiber.NewError(fiber.StatusNotFound, "tenant not found")
	}

	var req updateTenantRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	tenant, err := h.tenantRepo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "tenant not found")
	}

	tenant.Name = req.Name
	tenant.UpdatedAt = time.Now().UTC()
	if err := h.tenantRepo.Update(reqCtx(c), tenant); err != nil {
		return err
	}
	h.activity.Record(reqCtx(c), activity.CategoryAuthentication, "tenant_updated",
		activity.WithEntity("tenant", tenant.ID), activity.WithSource(activity.SourceAPI))
	return c.JSON(tenant)
}

// callerTenantID returns the tenant id of the authenticated caller, read from
// the request context (set by RequireAuth via tenantctx.Key), with the session
// local as a defensive fallback.
func (h *TenantsHandler) callerTenantID(c *fiber.Ctx) string {
	if id, ok := tenantctx.From(reqCtx(c)); ok {
		return id
	}
	if s, ok := c.Locals("session").(*models.Session); ok && s != nil {
		return s.TenantID
	}
	return ""
}

func (h *TenantsHandler) respondTenant(c *fiber.Ctx, id string) error {
	tenant, err := h.tenantRepo.GetByID(reqCtx(c), id)
	if err != nil {
		return notFound(err, "tenant not found")
	}
	return c.JSON(tenant)
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify produces a lowercase, URL-safe label from a tenant name.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "tenant"
	}
	return s
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}

// isUniqueViolationOn reports whether err is a unique-constraint violation on
// the specifically-named constraint. Callers that special-case one constraint
// use this so an unrelated unique conflict follows the normal error path.
func isUniqueViolationOn(err error, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
