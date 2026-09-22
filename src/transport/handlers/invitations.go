package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Invitation rate-limit budget (CON-26). Creating an invite sends mail on
// demand, so it is throttled per workspace (so one workspace can't be used to
// blast mail) and per client IP (so one caller can't spray across workspaces).
// Accept is public and does argon2 work, so it is throttled per IP to blunt
// token-guessing — the tokens are 256-bit, so this is defence-in-depth.
const (
	invitePerTenantBurst = 20
	invitePerIPBurst     = 30
	inviteAcceptPerIP    = 30
	inviteRateWindow     = time.Hour
)

// inviteThrottledMsg / invitationInvalidMsg are surfaced to the client verbatim.
// The invalid message is deliberately one indistinguishable sentence for every
// unusable token — unknown, expired, revoked, or already accepted — so accept is
// not an oracle (mirrors the password-reset contract, CON-161).
const (
	inviteThrottledMsg   = "Too many invitations. Please wait a minute and try again."
	invitationInvalidMsg = "This invitation link is invalid or has expired."
)

// errInvitationInvalid is the internal sentinel the accept transaction returns
// for any unusable token; it maps to a 410 + invitationInvalidMsg.
var errInvitationInvalid = errors.New("invitation invalid")

// InvitationEmailEnqueuer enqueues the transactional invitation email inside the
// invite-minting transaction, so the mail exists iff the invitation row does
// (CON-26). Implemented by *queues.Enqueuer; a narrow interface keeps this
// handler out of the jobs package and unit-testable.
type InvitationEmailEnqueuer interface {
	EnqueueInvitationEmailTx(ctx context.Context, tx *sql.Tx, tenantID, invitationID, toEmail, inviteURL, inviterName, workspaceName, role string) error
}

// InvitationsHandler serves workspace invitations (CON-26/CON-147): owner-gated
// create/list/revoke, and the public preview/accept the invitee uses. Accepting
// adds a membership of the invite's workspace — creating the account + a session
// for a brand-new email, or attaching to an existing account (signed in as it)
// with no new credentials. Owners invite into the ACTIVE workspace, resolved
// per request from the X-Workspace-Id header (CON-147 PR2).
type InvitationsHandler struct {
	db           *bun.DB
	userRepo     repository.UserRepository
	accountRepo  repository.AccountRepository
	tenantRepo   repository.TenantRepository
	inviteRepo   repository.InvitationRepository
	sessionRepo  repository.SessionRepository
	emailJobs    InvitationEmailEnqueuer
	appBaseURL   string
	cookieName   string
	secureCookie bool
	auth         fiber.Handler

	tenantLimiter *keyedRateLimiter
	ipLimiter     *keyedRateLimiter
	acceptLimiter *keyedRateLimiter

	limiter  *entitlements.Limiter // CON-295 entitlement quota gate (nil-safe)
	activity *activity.Recorder
}

// SetLimiter wires the CON-295 entitlement limiter (nil-safe no-op). Invites are
// the preferred way to add teammates, so the team_seats cap must gate the accept
// path as well as the direct-create path in users.go (CON-295 §5).
func (h *InvitationsHandler) SetLimiter(l *entitlements.Limiter) { h.limiter = l }

// seatQuota checks the team_seats entitlement for the workspace an invite joins.
// Accepting is unauthenticated (acceptNew) or signed in as a different workspace
// (acceptExisting), so the seat counter — which reads the tenant from the context
// (userRepository.CountInTenant) — is scoped explicitly to the invite's tenant;
// without this the count would span every tenant and wrongly block.
func (h *InvitationsHandler) seatQuota(ctx context.Context, tenantID string) (entitlements.Decision, error) {
	return h.limiter.Require(tenantctx.With(ctx, tenantID), tenantID, "team_seats")
}

// NewInvitationsHandler builds the handler. appBaseURL (APP_BASE_URL) is the base
// for the emailed accept link; cookieName/secureCookie mirror the session cookie
// set at login so accept can auto-log-in the new user.
func NewInvitationsHandler(db *bun.DB, userRepo repository.UserRepository, accountRepo repository.AccountRepository, tenantRepo repository.TenantRepository, inviteRepo repository.InvitationRepository, sessionRepo repository.SessionRepository, appBaseURL, cookieName string, secureCookie bool, auth fiber.Handler) *InvitationsHandler {
	return &InvitationsHandler{
		db:            db,
		userRepo:      userRepo,
		accountRepo:   accountRepo,
		tenantRepo:    tenantRepo,
		inviteRepo:    inviteRepo,
		sessionRepo:   sessionRepo,
		appBaseURL:    appBaseURL,
		cookieName:    cookieName,
		secureCookie:  secureCookie,
		auth:          auth,
		tenantLimiter: newKeyedRateLimiter(invitePerTenantBurst, inviteRateWindow),
		ipLimiter:     newKeyedRateLimiter(invitePerIPBurst, inviteRateWindow),
		acceptLimiter: newKeyedRateLimiter(inviteAcceptPerIP, inviteRateWindow),
	}
}

// SetActivityRecorder wires the CON-125 activity recorder (nil-safe no-op).
func (h *InvitationsHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

// SetEmailEnqueuer wires the CON-26 invitation-email enqueuer (nil-safe no-op).
// When unset, creating an invite mints + stores it but sends no mail.
func (h *InvitationsHandler) SetEmailEnqueuer(e InvitationEmailEnqueuer) { h.emailJobs = e }

func (h *InvitationsHandler) Register(app *fiber.App) {
	// Public accept surface — the emailed token is the capability, so these are
	// unauthenticated. Registered BEFORE the auth-gated group below: the group's
	// auth middleware is matched by the /api/invitations prefix, so anything
	// registered after it under that prefix would inherit auth (mirrors the
	// signup-before-group ordering in tenants.go).
	app.Get("/api/invitations/accept/:token", h.Preview)
	app.Post("/api/invitations/accept/:token", h.Accept)

	// Management: owner-gated inside each handler (via requireOwner).
	g := app.Group("/api/invitations", h.auth)
	g.Post("/", h.Create)
	g.Get("/", h.List)
	g.Delete("/:id", h.Revoke)
}

type createInvitationRequest struct {
	Email string `json:"email" validate:"required,email"`
	// Role defaults to member; an owner may invite a co-owner by passing "owner".
	Role string `json:"role" validate:"omitempty,oneof=owner member"`
}

// Create godoc
// @Summary      Invite a teammate
// @Description  Owner-only. Emails a single-use invitation link to join the caller's active workspace with the given role (default member). Idempotent per email (CON-147 §7.3): re-inviting an address with a pending invite — live or expired — re-issues it with a fresh token, expiry and email and returns 200 (a brand-new invite returns 201); there is no separate resend endpoint. An existing Ogen account may be invited into another workspace; 409 only if the email is already a member of THIS workspace. Rate-limited per workspace and per IP (CON-26/CON-147).
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      createInvitationRequest  true  "Invitation payload"
// @Success      201   {object}  models.Invitation  "New invitation created"
// @Success      200   {object}  models.Invitation  "Existing pending invitation re-issued"
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Failure      429   {object}  map[string]string
// @Router       /api/invitations [post]
func (h *InvitationsHandler) Create(c *fiber.Ctx) error {
	caller, err := requireOwner(c, h.userRepo)
	if err != nil {
		return err
	}

	var req createInvitationRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	email := repository.NormalizeEmail(req.Email)
	role := req.Role
	if role == "" {
		role = models.RoleMember
	}
	tenantID := caller.TenantID

	// Each invite sends mail, so throttle before doing the work. IP first, so a
	// throttled caller doesn't also spend the workspace's budget.
	if ok, retry := h.ipLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, inviteThrottledMsg)
	}
	if ok, retry := h.tenantLimiter.allow(tenantID); !ok {
		return tooManyRequests(c, retry, inviteThrottledMsg)
	}

	// An existing Ogen account CAN be invited into another workspace (CON-147 PR3:
	// accept attaches a membership to it). The only thing barred is inviting
	// someone who is already a member of THIS workspace — that is the 409, scoped
	// to the tenant rather than to "has an account anywhere." Resolved against
	// accounts (identity) since users.email is no longer unique. The caller is an
	// authenticated owner, so surfacing membership state is acceptable.
	if acct, err := h.accountRepo.GetByEmail(c.Context(), email); err == nil {
		if _, merr := h.userRepo.GetMembership(c.Context(), acct.ID, tenantID); merr == nil {
			return fiber.NewError(fiber.StatusConflict, "this email is already a member of this workspace")
		} else if !errors.Is(merr, sql.ErrNoRows) {
			return merr
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().UTC()

	token, hash, err := models.NewInvitationToken()
	if err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	inv := &models.Invitation{
		ID:        id,
		TenantID:  tenantID,
		Email:     email,
		Role:      role,
		TokenHash: hash,
		InvitedBy: caller.ID,
		Status:    models.InvitationPending,
		ExpiresAt: now.Add(models.InvitationTTL),
		CreatedAt: now,
	}
	inviteURL := strings.TrimRight(h.appBaseURL, "/") + "/invite?token=" + token

	// The email needs the workspace name; the invitee has no user/tenant to load
	// at send time, so resolve it here and pass it as a template var.
	workspaceName := ""
	if t, terr := h.tenantRepo.GetByID(c.Context(), tenantID); terr == nil && t != nil {
		workspaceName = t.Name
	}

	// Invite row + email enqueue commit together: a rolled-back tx queues no mail,
	// a committed one queues exactly one link resolving to a stored hash (mirrors
	// password-reset dispatch, CON-161). CreateReplacingPendingTx clears any
	// pending invite (live or expired) that holds the partial-unique slot and
	// reports whether it replaced one, so re-inviting is idempotent (CON-147 §7.3).
	var reissued bool
	if err := h.db.RunInTx(c.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		replaced, err := h.inviteRepo.CreateReplacingPendingTx(ctx, tx, inv)
		if err != nil {
			return err
		}
		reissued = replaced
		if h.emailJobs != nil {
			return h.emailJobs.EnqueueInvitationEmailTx(ctx, tx.Tx, tenantID, id, email, inviteURL, caller.Name, workspaceName, role)
		}
		return nil
	}); err != nil {
		// Two concurrent invites for the same address race the delete+insert; the
		// loser hits the partial unique index. Surface that as 409, not a raw 500 —
		// the caller can retry, which then re-issues cleanly.
		if isUniqueViolation(err) {
			return fiber.NewError(fiber.StatusConflict, "an invitation for this email is already pending")
		}
		return err
	}

	h.activity.Record(
		logging.WithUserID(tenantctx.With(c.Context(), tenantID), caller.ID),
		activity.CategoryAuthentication, "member_invited",
		activity.WithEntity("invitation", id), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"role": role}),
	)
	// 200 when an existing pending invite was re-issued, 201 for a brand-new one.
	status := fiber.StatusCreated
	if reissued {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(inv)
}

// List godoc
// @Summary      List invitations
// @Description  Owner-only. Lists the caller's workspace invitations, newest first (CON-26).
// @Tags         invitations
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   models.Invitation
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Router       /api/invitations [get]
func (h *InvitationsHandler) List(c *fiber.Ctx) error {
	caller, err := requireOwner(c, h.userRepo)
	if err != nil {
		return err
	}
	invs, err := h.inviteRepo.ListByTenant(c.Context(), caller.TenantID)
	if err != nil {
		return err
	}
	return c.JSON(invs)
}

// Revoke godoc
// @Summary      Revoke an invitation
// @Description  Owner-only. Revokes a pending invitation in the caller's workspace, killing its link (CON-26).
// @Tags         invitations
// @Security     CookieAuth
// @Param        id   path  string  true  "Invitation id"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/invitations/{id} [delete]
func (h *InvitationsHandler) Revoke(c *fiber.Ctx) error {
	caller, err := requireOwner(c, h.userRepo)
	if err != nil {
		return err
	}
	ok, err := h.inviteRepo.Revoke(c.Context(), c.Params("id"), caller.TenantID)
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "invitation not found")
	}
	h.activity.Record(
		logging.WithUserID(tenantctx.With(c.Context(), caller.TenantID), caller.ID),
		activity.CategoryAuthentication, "invitation_revoked",
		activity.WithEntity("invitation", c.Params("id")), activity.WithSource(activity.SourceAPI),
	)
	return c.SendStatus(fiber.StatusNoContent)
}

type invitationPreviewResponse struct {
	WorkspaceName string `json:"workspace_name"`
	InviterName   string `json:"inviter_name"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	// HasAccount tells the accept page which mode applies without a wasted POST:
	// true → the invited email already has an Ogen account, so prompt "sign in as
	// <email>" (no password field) and accept; false → collect name + password to
	// create the account. The invitee already holds the capability token and sees
	// their own email here, so disclosing whether that one address is registered
	// adds no meaningful enumeration surface (CON-147).
	HasAccount bool      `json:"has_account"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Preview godoc
// @Summary      Preview an invitation
// @Description  Public. Returns the workspace, inviter, invited email, role, and has_account (whether the invited email already has an Ogen account, so the accept page can prompt sign-in vs name+password) for a valid, pending, unexpired invitation token; every unusable token returns the same generic 410 so the endpoint is not an oracle (CON-26/CON-147).
// @Tags         invitations
// @Produce      json
// @Param        token  path  string  true  "Invitation token"
// @Success      200  {object}  invitationPreviewResponse
// @Failure      410  {object}  map[string]string
// @Router       /api/invitations/accept/{token} [get]
func (h *InvitationsHandler) Preview(c *fiber.Ctx) error {
	inv, err := h.inviteRepo.GetByTokenHash(c.Context(), models.HashInvitationToken(c.Params("token")))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unknown, expired, and revoked tokens all answer with the same 410 +
			// message (matching Accept), so preview isn't a token-existence oracle.
			return fiber.NewError(fiber.StatusGone, invitationInvalidMsg)
		}
		return err
	}
	if inv.Status != models.InvitationPending || time.Now().UTC().After(inv.ExpiresAt) {
		return fiber.NewError(fiber.StatusGone, invitationInvalidMsg)
	}

	// The invite authorizes reading its own workspace's display data; scope the
	// tenant-scoped reads to the invite's tenant. Both are best-effort — a missing
	// name just renders blank on the accept page.
	ctx := tenantctx.With(c.Context(), inv.TenantID)
	resp := invitationPreviewResponse{Email: inv.Email, Role: inv.Role, ExpiresAt: inv.ExpiresAt}
	if t, terr := h.tenantRepo.GetByID(ctx, inv.TenantID); terr == nil && t != nil {
		resp.WorkspaceName = t.Name
	}
	if u, uerr := h.userRepo.GetByID(ctx, inv.InvitedBy); uerr == nil && u != nil {
		resp.InviterName = u.Name
	}
	// Does the invited address already have an account? Resolved against accounts
	// (identity), not users (membership), since the same email may hold memberships
	// in several workspaces. A lookup error other than "no such account" is fatal;
	// sql.ErrNoRows simply leaves has_account false (a new-account invite).
	if _, aerr := h.accountRepo.GetByEmail(c.Context(), inv.Email); aerr == nil {
		resp.HasAccount = true
	} else if !errors.Is(aerr, sql.ErrNoRows) {
		return aerr
	}
	return c.JSON(resp)
}

// acceptInvitationRequest carries the new-account credentials. They are required
// only when the invited email has no Ogen account yet; an existing account
// accepts while logged in and supplies neither (CON-147 PR3), so both are
// optional at the schema level and enforced per path.
type acceptInvitationRequest struct {
	Name     string `json:"name"     validate:"omitempty"`
	Password string `json:"password" validate:"omitempty,min=8"`
}

type acceptInvitationResponse struct {
	Tenant *models.Tenant `json:"tenant,omitempty"`
	User   *models.User   `json:"user"`
	// Session is set only on the new-account path (auto-login); an existing
	// account is already signed in, so it accepts without a new session.
	Session *models.Session `json:"session,omitempty"`
}

// Accept godoc
// @Summary      Accept an invitation
// @Description  Public. Consumes a single-use invitation token and joins its workspace with the invited role. If the invited email has no Ogen account, the body's name+password create one and a session is opened (auto-login, 201). If the email already has an account, the caller must be signed in as it and only a membership is added (200, no new session). Returns a generic 410 for any unusable token.
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Param        token  path  string                    true  "Invitation token"
// @Param        body   body  acceptInvitationRequest   true  "Name + password (new accounts only)"
// @Success      201  {object}  acceptInvitationResponse  "New account created and logged in"
// @Success      200  {object}  acceptInvitationResponse  "Membership added to an existing account"
// @Failure      400  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      410  {object}  map[string]string
// @Failure      429  {object}  map[string]string
// @Router       /api/invitations/accept/{token} [post]
func (h *InvitationsHandler) Accept(c *fiber.Ctx) error {
	// Throttle the public, argon2-doing path per IP before any work.
	if ok, retry := h.acceptLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, inviteThrottledMsg)
	}

	var req acceptInvitationRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	// Peek the invite WITHOUT consuming, to choose the path: a new email needs
	// credentials and gets a session; an existing account is attached while logged
	// in. The atomic single-use consume happens inside the path's transaction, so
	// a token spent between here and there still yields one indistinguishable 410,
	// and a rejected accept (bad credentials / not signed in) leaves it unspent.
	inv, err := h.inviteRepo.GetByTokenHash(c.Context(), models.HashInvitationToken(c.Params("token")))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusGone, invitationInvalidMsg)
		}
		return err
	}
	if inv.Status != models.InvitationPending || time.Now().UTC().After(inv.ExpiresAt) {
		return fiber.NewError(fiber.StatusGone, invitationInvalidMsg)
	}

	account, err := h.accountRepo.GetByEmail(c.Context(), inv.Email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if account != nil {
		return h.acceptExisting(c, inv, account)
	}
	return h.acceptNew(c, inv, req)
}

// acceptExisting attaches a membership to an account that already owns the
// invited address — no new credentials, no new session (CON-147 PR3). The caller
// must be signed in as that account, so a stray link can't graft a workspace
// onto someone else's identity; if they aren't, the invite is left unconsumed so
// they can log in and retry.
func (h *InvitationsHandler) acceptExisting(c *fiber.Ctx, inv *models.Invitation, account *models.Account) error {
	if h.callerAccountID(c) != account.ID {
		return fiber.NewError(fiber.StatusForbidden, "sign in as "+inv.Email+" to accept this invitation")
	}
	// Already a member? The unique(tenant, account) index is the hard backstop;
	// this is the friendly pre-check.
	if _, err := h.userRepo.GetMembership(c.Context(), account.ID, inv.TenantID); err == nil {
		return fiber.NewError(fiber.StatusConflict, "you are already a member of this workspace")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// CON-295: attaching an existing account is still a new seat, so the
	// team_seats quota gates it exactly as acceptNew, before the token is consumed.
	seatDec, err := h.seatQuota(c.Context(), inv.TenantID)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	uid, err := models.NewID()
	if err != nil {
		return err
	}
	membership := &models.User{ID: uid, AccountID: account.ID, TenantID: inv.TenantID, Name: account.Name, Email: account.Email, Role: inv.Role, CreatedAt: now, UpdatedAt: now}

	if err := h.db.RunInTx(c.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, cerr := h.inviteRepo.ConsumeByTokenTx(ctx, tx, inv.TokenHash, now); cerr != nil {
			if errors.Is(cerr, sql.ErrNoRows) {
				return errInvitationInvalid
			}
			return cerr
		}
		return h.userRepo.CreateTx(ctx, tx, membership)
	}); err != nil {
		if errors.Is(err, errInvitationInvalid) {
			return fiber.NewError(fiber.StatusGone, invitationInvalidMsg)
		}
		if isUniqueViolation(err) {
			return fiber.NewError(fiber.StatusConflict, "you are already a member of this workspace")
		}
		return err
	}

	h.activity.Record(
		logging.WithUserID(tenantctx.With(c.Context(), inv.TenantID), uid),
		activity.CategoryAuthentication, "invitation_accepted",
		activity.WithEntity("user", uid), activity.WithSource(activity.SourceAPI),
	)
	// CON-295 §12: the seat is now taken — fire any near-limit crossing.
	h.limiter.DispatchCrossing(tenantctx.With(c.Context(), inv.TenantID), inv.TenantID, seatDec)

	// No cookie: the caller stays in whatever workspace their session is on; the
	// client offers to switch to the newly joined one.
	resp := acceptInvitationResponse{User: membership}
	if t, terr := h.tenantRepo.GetByID(c.Context(), inv.TenantID); terr == nil {
		resp.Tenant = t
	}
	return c.Status(fiber.StatusOK).JSON(resp)
}

// acceptNew creates the identity (account), the membership, and a session for a
// brand-new invited email (auto-login). Name + password are required and checked
// before the token is consumed, so a bad password doesn't burn a still-valid
// invite.
func (h *InvitationsHandler) acceptNew(c *fiber.Ctx, inv *models.Invitation, req acceptInvitationRequest) error {
	if req.Name == "" || len(req.Password) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "name and a password of at least 8 characters are required")
	}
	// CON-295: the team_seats quota gates the workspace gaining a member. Require
	// returns a *QuotaExceededError (→ 402) when at cap in enforce mode. Checked
	// before the token is consumed so an over-cap workspace leaves the invite
	// valid (the owner can free a seat or upgrade, then the invitee retries).
	seatDec, err := h.seatQuota(c.Context(), inv.TenantID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	var (
		newUser *models.User
		session *models.Session
	)
	err = h.db.RunInTx(c.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, cerr := h.inviteRepo.ConsumeByTokenTx(ctx, tx, inv.TokenHash, now); cerr != nil {
			if errors.Is(cerr, sql.ErrNoRows) {
				return errInvitationInvalid
			}
			return cerr
		}
		uid, err := models.NewID()
		if err != nil {
			return err
		}
		aid, err := models.NewID()
		if err != nil {
			return err
		}
		hash, err := models.HashPassword(req.Password)
		if err != nil {
			return err
		}
		// accounts.email is the uniqueness backstop: if the address gained an
		// account between the peek and here (a raced signup), this insert fires it
		// and the whole tx rolls back — the invite is left unconsumed and the caller
		// can log in as that account and accept again.
		account := &models.Account{ID: aid, Email: inv.Email, PasswordHash: hash, Name: req.Name, CreatedAt: now, UpdatedAt: now}
		if err := h.accountRepo.CreateTx(ctx, tx, account); err != nil {
			return err
		}
		newUser = &models.User{ID: uid, AccountID: aid, TenantID: inv.TenantID, Name: req.Name, Email: inv.Email, Role: inv.Role, CreatedAt: now, UpdatedAt: now}
		if err := h.userRepo.CreateTx(ctx, tx, newUser); err != nil {
			return err
		}
		tokenStr, err := models.NewSessionToken()
		if err != nil {
			return err
		}
		session = &models.Session{ID: tokenStr, AccountID: aid, UserID: uid, TenantID: inv.TenantID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now}
		return h.sessionRepo.CreateTx(ctx, tx, session)
	})
	if err != nil {
		if errors.Is(err, errInvitationInvalid) {
			return fiber.NewError(fiber.StatusGone, invitationInvalidMsg)
		}
		if isUniqueViolation(err) {
			return fiber.NewError(fiber.StatusConflict, "this email already has an Ogen account — sign in to accept")
		}
		return err
	}

	c.Cookie(&fiber.Cookie{
		Name:     h.cookieName,
		Value:    session.ID,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HTTPOnly: true,
		Secure:   h.secureCookie,
		SameSite: "Lax",
	})

	h.activity.Record(
		logging.WithUserID(tenantctx.With(c.Context(), inv.TenantID), newUser.ID),
		activity.CategoryAuthentication, "invitation_accepted",
		activity.WithEntity("user", newUser.ID), activity.WithSource(activity.SourceAPI),
	)
	// CON-295 §12: the member now exists — fire any near-limit crossing to the
	// workspace owners (tenant-scoped ctx so the notifier resolves the invite's
	// tenant, not the acceptor's absent one).
	h.limiter.DispatchCrossing(tenantctx.With(c.Context(), inv.TenantID), inv.TenantID, seatDec)

	resp := acceptInvitationResponse{User: newUser, Session: session}
	if t, terr := h.tenantRepo.GetByID(c.Context(), inv.TenantID); terr == nil {
		resp.Tenant = t
	}
	return c.Status(fiber.StatusCreated).JSON(resp)
}

// callerAccountID returns the account id of the session cookie on the request,
// or "" when there is no valid, unexpired session. Used by the public accept
// endpoint to check whether the caller is signed in as the invited account
// (it runs outside the auth middleware, so it resolves the session by hand).
func (h *InvitationsHandler) callerAccountID(c *fiber.Ctx) string {
	token := c.Cookies(h.cookieName)
	if token == "" {
		return ""
	}
	s, err := h.sessionRepo.GetByID(c.Context(), token)
	if err != nil || s == nil || time.Now().UTC().After(s.ExpiresAt) {
		return ""
	}
	return s.AccountID
}
