package handlers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const sessionTTL = 7 * 24 * time.Hour

// Login throttle budget (CON-162). POST /api/sessions is unauthenticated and
// globally addressable (email is unique across tenants), so it is the prime
// credential-stuffing target. It is charged per client IP (stops one caller
// spraying many addresses) and per address (stops a targeted brute force).
// Both count failures only — a successful login costs nothing — and the
// per-address burst is deliberately more generous than reset's 5 so a human
// mistyping a password a few times isn't shut out; the per-IP budget does the
// heavy lifting against distribution.
const (
	loginPerEmailBurst = 10
	loginPerIPBurst    = 30
	loginRateWindow    = time.Hour
)

// loginThrottledMsg is the 429 body for a throttled login. It reads as a
// sentence because the login form surfaces it verbatim, and it is identical
// whether or not the address exists, so a 429 never signals account existence.
const loginThrottledMsg = "Too many login attempts. Please wait a minute and try again."

// maxConcurrentThrottleRecords bounds the best-effort login_throttled recorder
// (CON-162). Every throttled login that might target an account spends one slot
// on a detached GetByEmail + activity write; when the slots are all busy the
// event is dropped without starting another lookup, so a flood on the throttle
// path can't spawn unbounded goroutines or DB queries. A handful is ample — the
// event is a visibility signal, not an audit log, and the activity recorder
// itself drops when its own buffer fills.
const maxConcurrentThrottleRecords = 8

type SessionsHandler struct {
	userRepo     repository.UserRepository
	accountRepo  repository.AccountRepository
	sessionRepo  repository.SessionRepository
	cookieName   string
	secureCookie bool
	// ipLimiter / emailLimiter throttle failed logins per client IP and per
	// address (CON-162). Both are charged only on failure and the address bucket
	// is refunded on success, so only credential-guessing accrues against them.
	ipLimiter    *keyedRateLimiter
	emailLimiter *keyedRateLimiter
	// throttleRecordSem is a counting semaphore bounding in-flight login_throttled
	// recordings (CON-162), so the async recorder can't be amplified into an
	// unbounded goroutine/DB-lookup fan-out under a login flood.
	throttleRecordSem chan struct{}
	// activity records CON-125 authentication events (login, logout). Both run
	// outside tenant scope, so the handler builds an explicit tenant+user
	// context per event. nil is a no-op. Wired via SetActivityRecorder.
	activity *activity.Recorder
}

func NewSessionsHandler(userRepo repository.UserRepository, accountRepo repository.AccountRepository, sessionRepo repository.SessionRepository, cookieName string, secureCookie bool) *SessionsHandler {
	return &SessionsHandler{
		userRepo:          userRepo,
		accountRepo:       accountRepo,
		sessionRepo:       sessionRepo,
		cookieName:        cookieName,
		secureCookie:      secureCookie,
		ipLimiter:         newKeyedRateLimiter(loginPerIPBurst, loginRateWindow),
		emailLimiter:      newKeyedRateLimiter(loginPerEmailBurst, loginRateWindow),
		throttleRecordSem: make(chan struct{}, maxConcurrentThrottleRecords),
	}
}

// SetActivityRecorder wires the CON-125 activity recorder (nil-safe no-op).
func (h *SessionsHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

// authCtx builds a context carrying the given tenant + user so the recorder can
// attribute an auth event that happens before the auth middleware would run.
func (h *SessionsHandler) authCtx(c *fiber.Ctx, tenantID, userID string) context.Context {
	return logging.WithUserID(tenantctx.With(reqCtx(c), tenantID), userID)
}

func (h *SessionsHandler) Register(app *fiber.App) {
	g := app.Group("/api/sessions")
	g.Post("/", h.Create)
	g.Delete("/", h.Delete)
}

type createSessionRequest struct {
	Email    string `json:"email"    validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

// Create godoc
// @Summary      Login
// @Description  Authenticates a user by email and password, sets a session cookie, and returns the created session. Failed attempts are rate-limited per client IP and per address; once a budget is exhausted the endpoint answers 429 with a Retry-After header (CON-162).
// @Tags         sessions
// @Accept       json
// @Produce      json
// @Param        body  body      createSessionRequest  true  "Credentials"
// @Success      201   {object}  models.Session
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      429   {object}  map[string]string
// @Router       /api/sessions [post]
func (h *SessionsHandler) Create(c *fiber.Ctx) error {
	var req createSessionRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	// Throttle before any credential work. The IP budget is checked first so a
	// caller spraying many addresses doesn't also spend the target address's
	// budget (CON-162). Both are only peeked here — a token is charged just on a
	// failed attempt (loginFailed), so a legitimate login never accrues. A 429
	// reveals only that a limit was hit, never whether the address has an account.
	// Either dimension records the throttle for a known account (off-path, so it
	// stays silent for unknown addresses and never blocks the response), so a
	// targeted attempt is visible even when spraying trips the IP budget first.
	if retry := h.ipLimiter.retryAfter(c.IP()); retry > 0 {
		h.recordLoginThrottled(c, req.Email)
		return tooManyRequests(c, retry, loginThrottledMsg)
	}
	if retry := h.emailLimiter.retryAfter(req.Email); retry > 0 {
		h.recordLoginThrottled(c, req.Email)
		return tooManyRequests(c, retry, loginThrottledMsg)
	}

	// Authenticate the ACCOUNT (identity), which holds the credential since
	// CON-147 PR1. The membership the session lands on is resolved from it below.
	account, err := h.accountRepo.GetByEmail(reqCtx(c), req.Email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return h.loginFailed(c, req.Email)
		}
		return err
	}

	ok, err := models.VerifyPassword(req.Password, account.PasswordHash)
	if err != nil || !ok {
		return h.loginFailed(c, req.Email)
	}

	// Resolve the workspace membership this login opens into — the oldest LIVE one
	// (GetByAccountID skips soft-deleted workspaces, CON-147 PR4). An account with
	// no live workspace (e.g. removed from its last one) has nothing to open a
	// session onto; answer 403 rather than a 500.
	user, err := h.userRepo.GetByAccountID(reqCtx(c), account.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusForbidden, "your account has no active workspace")
		}
		return err
	}

	// The address proved control of the credential, so refund its bucket — a user
	// who fumbled their password a few times before getting it right starts clean
	// (CON-162: a success resets the counter). The IP bucket is left as-is: it
	// guards against spraying, which one success doesn't clear (and clearing it
	// would let an attacker holding one valid login reset their own IP budget).
	h.emailLimiter.reset(req.Email)

	token, err := models.NewSessionToken()
	if err != nil {
		return err
	}

	session := &models.Session{
		ID:        token,
		AccountID: account.ID,
		UserID:    user.ID,
		TenantID:  user.TenantID,
		ExpiresAt: time.Now().UTC().Add(sessionTTL),
	}
	if err := h.sessionRepo.Create(reqCtx(c), session); err != nil {
		return err
	}

	h.activity.Record(h.authCtx(c, session.TenantID, user.ID), activity.CategoryAuthentication, "login",
		activity.WithEntity("user", user.ID), activity.WithSource(activity.SourceAPI))

	c.Cookie(&fiber.Cookie{
		Name:     h.cookieName,
		Value:    token,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HTTPOnly: true,
		Secure:   h.secureCookie,
		SameSite: "Lax",
	})

	return c.Status(fiber.StatusCreated).JSON(session)
}

// loginFailed charges the failed attempt against both the per-IP and per-address
// budgets and returns the generic 401 (CON-162). The body is identical whether
// the address is unknown or the password is wrong, so login is not an
// account-existence oracle.
func (h *SessionsHandler) loginFailed(c *fiber.Ctx, email string) error {
	h.ipLimiter.penalize(c.IP())
	h.emailLimiter.penalize(email)
	return fiber.NewError(fiber.StatusUnauthorized, "invalid credentials")
}

// recordLoginThrottled emits a CON-125 authentication event when a throttled
// login targets a real account, so a targeted brute force shows up in the
// activity feed. The lookup + emit run on a detached goroutine so they stay off
// the 429 response path — the throttle response leaks nothing about account
// existence through timing, and an unknown address produces no event at all.
func (h *SessionsHandler) recordLoginThrottled(c *fiber.Ctx, email string) {
	if h.activity == nil {
		return
	}
	// Admit at most maxConcurrentThrottleRecords in-flight recordings; if every
	// slot is busy, drop this event here — before spawning a goroutine or touching
	// the database — so a flood on the throttle path stays bounded. The slot is
	// released when the recording finishes.
	select {
	case h.throttleRecordSem <- struct{}{}:
	default:
		return
	}
	ip := c.IP()
	go func() {
		defer func() { <-h.throttleRecordSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		account, err := h.accountRepo.GetByEmail(ctx, email)
		if err != nil || account == nil {
			return // unknown address ⇒ nothing to attribute
		}
		user, err := h.userRepo.GetByAccountID(ctx, account.ID)
		if err != nil || user == nil {
			return
		}
		h.activity.Record(
			logging.WithUserID(tenantctx.With(ctx, user.TenantID), user.ID),
			activity.CategoryAuthentication, "login_throttled",
			activity.WithEntity("user", user.ID), activity.WithSource(activity.SourceAPI),
			activity.WithPayload(map[string]any{"ip": ip}),
		)
	}()
}

// Delete godoc
// @Summary      Logout
// @Description  Invalidates the current session and clears the session cookie.
// @Tags         sessions
// @Security     CookieAuth
// @Success      204
// @Failure      401  {object}  map[string]string
// @Router       /api/sessions [delete]
func (h *SessionsHandler) Delete(c *fiber.Ctx) error {
	token := c.Cookies(h.cookieName)
	if token == "" {
		return fiber.NewError(fiber.StatusUnauthorized, "no session")
	}

	// Resolve the session's actor before deleting it, so the logout activity can
	// be attributed to the right tenant/user (this route has no auth middleware,
	// so the request context carries neither). Best-effort — a lookup miss just
	// skips attribution.
	var actorTenant, actorUser string
	if s, gerr := h.sessionRepo.GetByID(reqCtx(c), token); gerr == nil && s != nil {
		actorTenant, actorUser = s.TenantID, s.UserID
	}

	deleted, err := h.sessionRepo.Delete(reqCtx(c), token)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid session")
	}

	if actorTenant != "" {
		h.activity.Record(h.authCtx(c, actorTenant, actorUser), activity.CategoryAuthentication, "logout",
			activity.WithEntity("user", actorUser), activity.WithSource(activity.SourceAPI))
	}

	// Mirror the attributes used at login so the browser actually replaces the cookie.
	c.Cookie(&fiber.Cookie{
		Name:     h.cookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HTTPOnly: true,
		Secure:   h.secureCookie,
		SameSite: "Lax",
	})

	return c.SendStatus(fiber.StatusNoContent)
}
