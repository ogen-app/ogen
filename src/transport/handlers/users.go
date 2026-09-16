package handlers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

type UsersHandler struct {
	// db backs the password-change path, which must update the credential and
	// revoke the user's other sessions in one transaction (CON-193). Every other
	// operation goes through repo.
	db   *bun.DB
	repo repository.UserRepository
	// accountRepo backs the identity side of user management (CON-147): creating a
	// member creates their account, and password changes rotate the credential on
	// the account rather than the membership row.
	accountRepo repository.AccountRepository
	// settingRepo backed the setup_complete bootstrap gate, removed in CON-97
	// (signup via POST /api/tenants is the sole bootstrap). Retained on the
	// constructor to avoid churn across the ~37 call sites; revisit when
	// settings become per-tenant (PR3).
	settingRepo repository.SettingRepository
	auth        fiber.Handler
	// activity records CON-125 authentication-category events (user_created,
	// user_updated, user_deleted). nil is a no-op. Wired via SetActivityRecorder.
	activity *activity.Recorder
	limiter  *entitlements.Limiter // CON-295 entitlement quota gate (nil-safe)
}

func NewUsersHandler(db *bun.DB, repo repository.UserRepository, accountRepo repository.AccountRepository, settingRepo repository.SettingRepository, auth fiber.Handler) *UsersHandler {
	return &UsersHandler{db: db, repo: repo, accountRepo: accountRepo, settingRepo: settingRepo, auth: auth}
}

// SetActivityRecorder wires the CON-125 activity recorder (nil-safe no-op).
func (h *UsersHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

// SetLimiter wires the CON-295 entitlement limiter (nil-safe no-op).
func (h *UsersHandler) SetLimiter(l *entitlements.Limiter) { h.limiter = l }

func (h *UsersHandler) Register(app *fiber.App) {
	app.Get("/api/current_user", h.auth, h.CurrentUser) // always protected

	g := app.Group("/api/users")
	g.Post("/", h.auth, h.Create)           // owner-only; new users join the caller's tenant (CON-26/97)
	g.Get("/", h.auth, h.List)              // always protected
	g.Get("/:id", h.auth, h.Get)            // always protected
	g.Put("/:id", h.auth, h.Update)         // always protected
	g.Patch("/:id/role", h.auth, h.SetRole) // owner-only role change (CON-26)
	g.Delete("/:id", h.auth, h.Delete)      // self or owner (CON-26)
}

// requireSelf returns 403 unless the authenticated session belongs to the user
// identified by the :id route parameter. The auth middleware must have already
// stored the session in c.Locals("session").
func requireSelf(c *fiber.Ctx) error {
	session, ok := c.Locals("session").(*models.Session)
	if !ok || session == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if session.UserID != c.Params("id") {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}
	return nil
}

type createUserRequest struct {
	Name     string `json:"name"     validate:"required"`
	Email    string `json:"email"    validate:"required,email"`
	Password string `json:"password" validate:"required,min=8"`
	// Role is optional; it defaults to member. Only an owner may create users
	// (CON-26), so an owner can also mint a co-owner by passing "owner".
	Role string `json:"role" validate:"omitempty,oneof=owner member"`
}

// setRoleRequest is the body of PATCH /api/users/:id/role (CON-26).
type setRoleRequest struct {
	Role string `json:"role" validate:"required,oneof=owner member"`
}

// errCurrentPasswordMismatch is returned from inside the password-change
// transaction when the supplied current password fails re-verification, so the
// tx rolls back and the handler can map it to a 403 (CON-193 §1). Mirrors
// errResetInvalid in password_reset.go.
var errCurrentPasswordMismatch = errors.New("current password is incorrect")

type updateUserRequest struct {
	Name  string `json:"name"     validate:"required"`
	Email string `json:"email"    validate:"required,email"`
	// Password is optional on update; when provided it must be at least 8 characters.
	Password string `json:"password" validate:"omitempty,min=8"`
	// CurrentPassword re-authenticates a credential change (CON-193 §1): it is
	// required only when Password is present and is verified against the stored
	// hash, so it carries no min-length rule of its own. A plain name/email edit
	// leaves it empty and unchecked.
	CurrentPassword string `json:"current_password" validate:"required_with=Password"`
}

// CurrentUser godoc
// @Summary      Get current user
// @Description  Returns the authenticated user derived from the active session cookie.
// @Tags         users
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  models.User
// @Failure      401  {object}  map[string]string
// @Router       /api/current_user [get]
func (h *UsersHandler) CurrentUser(c *fiber.Ctx) error {
	session := c.Locals("session").(*models.Session)
	user, err := h.repo.GetByIDWithTenant(c.Context(), session.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusUnauthorized, "user not found")
		}
		return err
	}
	return c.JSON(user)
}

// List godoc
// @Summary      List users
// @Description  Returns all users ordered by creation date.
// @Tags         users
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   models.User
// @Failure      401  {object}  map[string]string
// @Router       /api/users [get]
func (h *UsersHandler) List(c *fiber.Ctx) error {
	users, err := h.repo.List(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(users)
}

// Create godoc
// @Summary      Create user
// @Description  Creates a new user in the authenticated caller's tenant (CON-97). Owner-only (CON-26); the new user defaults to the member role. Any tenant_id in the body is ignored. Email-based invitations (POST /api/invitations) are the preferred way to add teammates.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      createUserRequest  true  "User payload"
// @Success      201   {object}  models.User
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Router       /api/users [post]
func (h *UsersHandler) Create(c *fiber.Ctx) error {
	// Only an owner may add users directly (CON-26). Their tenant is the target;
	// a tenant_id in the request body is never trusted (CON-97 §7.2, §12.2).
	caller, err := requireOwner(c, h.repo)
	if err != nil {
		return err
	}

	// CON-295: the team_seats quota gates adding a member.
	seatDec, err := h.limiter.Require(c.Context(), caller.TenantID, "team_seats")
	if err != nil {
		return err
	}

	var req createUserRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}
	accountID, err := models.NewID()
	if err != nil {
		return err
	}

	hash, err := models.HashPassword(req.Password)
	if err != nil {
		return err
	}

	role := req.Role
	if role == "" {
		role = models.RoleMember
	}

	now := time.Now().UTC()
	// Directly adding a member creates their identity (account) and their
	// membership of the caller's workspace together (CON-147). PR3 replaces this
	// with invitations that can attach an existing account; here the email must be
	// new, enforced by accounts.email — a clash surfaces as 409.
	account := &models.Account{ID: accountID, Email: req.Email, PasswordHash: hash, Name: req.Name, CreatedAt: now, UpdatedAt: now}
	user := &models.User{
		ID:        id,
		AccountID: accountID,
		TenantID:  caller.TenantID,
		Name:      req.Name,
		Email:     req.Email,
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.db.RunInTx(c.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		if err := h.accountRepo.CreateTx(ctx, tx, account); err != nil {
			return err
		}
		return h.repo.CreateTx(ctx, tx, user)
	}); err != nil {
		if isUniqueViolation(err) {
			return fiber.NewError(fiber.StatusConflict, "email already in use")
		}
		return err
	}

	h.activity.Record(c.Context(), activity.CategoryAuthentication, "user_created",
		activity.WithEntity("user", user.ID), activity.WithSource(activity.SourceAPI))
	// CON-295: the member now exists — fire any near-limit crossing.
	h.limiter.DispatchCrossing(c.Context(), caller.TenantID, seatDec)
	return c.Status(fiber.StatusCreated).JSON(user)
}

// SetRole godoc
// @Summary      Change a member's role
// @Description  Owner-only. Sets a workspace member's role to owner or member (CON-26). The workspace must always keep at least one owner, so demoting the last owner returns 409.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string          true  "User Sqid"
// @Param        body  body      setRoleRequest  true  "Role payload"
// @Success      200   {object}  models.User
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/users/{id}/role [patch]
func (h *UsersHandler) SetRole(c *fiber.Ctx) error {
	caller, err := requireOwner(c, h.repo)
	if err != nil {
		return err
	}

	var req setRoleRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	// SetRoleGuarded scopes to the caller's tenant (404 for an outsider) and
	// enforces the >=1-owner invariant in one transaction (CON-26 §7/§11).
	updated, err := h.repo.SetRoleGuarded(c.Context(), c.Params("id"), caller.TenantID, req.Role)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fiber.NewError(fiber.StatusNotFound, "user not found")
		case errors.Is(err, repository.ErrLastOwner):
			return fiber.NewError(fiber.StatusConflict, "a workspace must have at least one owner")
		default:
			return err
		}
	}

	h.activity.Record(c.Context(), activity.CategoryAuthentication, "member_role_changed",
		activity.WithEntity("user", updated.ID), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"role": req.Role, "actor": caller.ID}))
	return c.JSON(updated)
}

// Get godoc
// @Summary      Get user
// @Description  Returns a single user by Sqid.
// @Tags         users
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "User Sqid"
// @Success      200  {object}  models.User
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/users/{id} [get]
func (h *UsersHandler) Get(c *fiber.Ctx) error {
	user, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "user not found")
	}
	return c.JSON(user)
}

// Update godoc
// @Summary      Update user
// @Description  Updates name and/or email of an existing user. When `password` is present the caller must also send `current_password`; it is re-verified and, on success, every other session for the user is revoked (the caller's own session is kept). CON-193.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string             true  "User Sqid"
// @Param        body  body      updateUserRequest  true  "User payload"
// @Success      200   {object}  models.User
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/users/{id} [put]
func (h *UsersHandler) Update(c *fiber.Ctx) error {
	if err := requireSelf(c); err != nil {
		return err
	}
	// requireSelf guarantees an authenticated session; hold on to it so a password
	// change can spare the caller's own session while revoking the rest (CON-193 §2).
	session := c.Locals("session").(*models.Session)

	var req updateUserRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "user not found")
	}

	if req.Password == "" {
		// Name/email-only edit: no re-authentication and no session revocation. The
		// membership row carries a denormalised copy, but email/name also live on
		// the account (identity) and login is by account email, so both are updated
		// together — a clash on the account email surfaces as 409 (CON-147).
		now := time.Now().UTC()
		if err := h.db.RunInTx(c.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
			user.Name = req.Name
			user.Email = req.Email
			user.UpdatedAt = now
			// Sync the denormalised copy on EVERY membership of this account, so the
			// person's name/email stays consistent across all their workspaces — not
			// just the one this request is scoped to.
			if _, err := tx.NewUpdate().Model((*models.User)(nil)).
				Set("name = ?", req.Name).Set("email = ?", req.Email).Set("updated_at = ?", now).
				Where("account_id = ?", user.AccountID).Exec(ctx); err != nil {
				return err
			}
			_, err := tx.NewUpdate().Model((*models.Account)(nil)).
				Set("name = ?", req.Name).Set("email = ?", req.Email).Set("updated_at = ?", now).
				Where("id = ?", user.AccountID).Exec(ctx)
			return err
		}); err != nil {
			if isUniqueViolation(err) {
				return fiber.NewError(fiber.StatusConflict, "email already in use")
			}
			return err
		}
	} else {
		// Password change. The credential lives on the account since CON-147, so lock
		// the ACCOUNT row, re-verify the current password against the *locked* hash,
		// rotate it, sync the membership's denormalised name/email, and revoke the
		// account's other sessions — all in one transaction. FOR UPDATE serializes
		// concurrent changes, so an in-flight request carrying the old (possibly
		// compromised) credential can't verify against a stale hash and slip through
		// after a rotation has already committed (CON-193 §1/§2). Mirrors
		// POST /api/password-reset/confirm, which likewise holds the row lock across
		// argon2 — password changes are rare, so hashing under the lock is fine. The
		// caller's own session (session.ID) is preserved so they aren't logged out of
		// the tab making the change.
		if err := h.db.RunInTx(c.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
			account := new(models.Account)
			if err := tx.NewSelect().Model(account).Where("a.id = ?", user.AccountID).For("UPDATE").Scan(ctx); err != nil {
				return err
			}
			ok, verr := models.VerifyPassword(req.CurrentPassword, account.PasswordHash)
			if verr != nil {
				return verr
			}
			if !ok {
				return errCurrentPasswordMismatch
			}
			hash, herr := models.HashPassword(req.Password)
			if herr != nil {
				return herr
			}
			now := time.Now().UTC()
			if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
				Set("password_hash = ?", hash).Set("name = ?", req.Name).Set("email = ?", req.Email).Set("updated_at = ?", now).
				Where("id = ?", user.AccountID).Exec(ctx); err != nil {
				return err
			}
			user.Name = req.Name
			user.Email = req.Email
			user.UpdatedAt = now
			// Sync the denormalised name/email on EVERY membership of this account
			// (see the name/email-only branch above).
			if _, err := tx.NewUpdate().Model((*models.User)(nil)).
				Set("name = ?", req.Name).Set("email = ?", req.Email).Set("updated_at = ?", now).
				Where("account_id = ?", user.AccountID).Exec(ctx); err != nil {
				return err
			}
			// Revoke the account's other sessions (a password change may be locking
			// out an intruder across every workspace the account can reach).
			_, err := tx.NewDelete().Model((*models.Session)(nil)).
				Where("account_id = ?", user.AccountID).
				Where("id != ?", session.ID).
				Exec(ctx)
			return err
		}); err != nil {
			if errors.Is(err, errCurrentPasswordMismatch) {
				return fiber.NewError(fiber.StatusForbidden, "current password is incorrect")
			}
			if isUniqueViolation(err) {
				return fiber.NewError(fiber.StatusConflict, "email already in use")
			}
			return err
		}
	}

	h.activity.Record(c.Context(), activity.CategoryAuthentication, "user_updated",
		activity.WithEntity("user", user.ID), activity.WithSource(activity.SourceAPI))
	return c.JSON(user)
}

// Delete godoc
// @Summary      Delete user
// @Description  Removes a workspace member. A user may remove themselves; an owner may remove anyone in the workspace (CON-26). The workspace must always keep at least one owner, so removing the last owner (including self) returns 409.
// @Tags         users
// @Security     CookieAuth
// @Param        id   path  string  true  "User Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/users/{id} [delete]
func (h *UsersHandler) Delete(c *fiber.Ctx) error {
	caller, err := callerUser(c, h.repo)
	if err != nil {
		return err
	}
	targetID := c.Params("id")
	// A member may only remove themselves; removing anyone else requires owner.
	if caller.ID != targetID && caller.Role != models.RoleOwner {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}

	// RemoveMemberGuarded scopes to the caller's tenant (404 for an outsider) and
	// enforces the >=1-owner invariant, so the last owner can't be removed —
	// including self-removal (CON-26 §7).
	if err := h.repo.RemoveMemberGuarded(c.Context(), targetID, caller.TenantID); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fiber.NewError(fiber.StatusNotFound, "user not found")
		case errors.Is(err, repository.ErrLastOwner):
			return fiber.NewError(fiber.StatusConflict, "a workspace must have at least one owner")
		default:
			return err
		}
	}

	// Distinguish self-removal from an owner removing a teammate (CON-26 §13).
	event := "user_deleted"
	if caller.ID != targetID {
		event = "member_removed"
	}
	h.activity.Record(c.Context(), activity.CategoryAuthentication, event,
		activity.WithEntity("user", targetID), activity.WithSource(activity.SourceAPI))
	return c.SendStatus(fiber.StatusNoContent)
}
