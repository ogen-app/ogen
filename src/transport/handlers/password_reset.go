package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Password-reset rate-limit budget (CON-161): the request endpoint sends mail on
// demand, so it is throttled both per address (so we can't be used to flood one
// inbox) and per client IP (so one caller can't spray many addresses). Both are
// generous enough for a human retrying, tight enough to stop abuse.
const (
	resetPerEmailBurst = 5
	resetPerIPBurst    = 20
	resetRateWindow    = time.Hour
)

// resetTokenInvalidMsg is returned for every unusable token — unknown, expired,
// or already spent — as one indistinguishable message, so confirm is not an
// oracle. It reads like a sentence because the UI shows it verbatim next to a
// "Request a new link" affordance (CON-161).
const resetTokenInvalidMsg = "This password reset link is invalid or has expired. Request a new one."

// errResetInvalid is the internal sentinel the confirm transaction returns for
// any unusable token; it is mapped to a 400 + resetTokenInvalidMsg.
var errResetInvalid = errors.New("password reset token invalid")

// PasswordResetEnqueuer enqueues the transactional password-reset email inside
// the token-minting transaction, so the mail exists iff the token row does
// (CON-161). Implemented by *queues.Enqueuer; a narrow interface keeps this
// handler out of the jobs package and unit-testable.
type PasswordResetEnqueuer interface {
	EnqueuePasswordResetTx(ctx context.Context, tx *sql.Tx, userID, tenantID, tokenID, resetURL string) error
}

// PasswordResetHandler serves the two public, unauthenticated password-reset
// endpoints (CON-161). The whole point is that the caller can't log in, so the
// emailed token is the only capability.
type PasswordResetHandler struct {
	db          *bun.DB
	userRepo    repository.UserRepository
	accountRepo repository.AccountRepository
	emailJobs   PasswordResetEnqueuer
	appBaseURL  string

	ipLimiter    *keyedRateLimiter
	emailLimiter *keyedRateLimiter

	// activity records CON-125 authentication events (password_reset_requested,
	// password_reset_completed). Both run outside tenant scope, so the handler
	// builds an explicit tenant+user context per event. nil is a no-op.
	activity *activity.Recorder
}

// NewPasswordResetHandler builds the handler. appBaseURL (APP_BASE_URL) is the
// base for the emailed reset link.
func NewPasswordResetHandler(db *bun.DB, userRepo repository.UserRepository, accountRepo repository.AccountRepository, appBaseURL string) *PasswordResetHandler {
	return &PasswordResetHandler{
		db:           db,
		userRepo:     userRepo,
		accountRepo:  accountRepo,
		appBaseURL:   appBaseURL,
		ipLimiter:    newKeyedRateLimiter(resetPerIPBurst, resetRateWindow),
		emailLimiter: newKeyedRateLimiter(resetPerEmailBurst, resetRateWindow),
	}
}

// SetActivityRecorder wires the CON-125 activity recorder (nil-safe no-op).
func (h *PasswordResetHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

// SetEmailEnqueuer wires the CON-161 reset-email enqueuer (nil-safe no-op).
// When unset, a reset request mints + stores a token but sends no mail.
func (h *PasswordResetHandler) SetEmailEnqueuer(e PasswordResetEnqueuer) { h.emailJobs = e }

func (h *PasswordResetHandler) Register(app *fiber.App) {
	// Both public and unauthenticated — the emailed token is the capability.
	app.Post("/api/password-reset", h.Request)
	app.Post("/api/password-reset/confirm", h.Confirm)
}

type passwordResetRequest struct {
	Email string `json:"email" validate:"required,email"`
}

// Request godoc
// @Summary      Request a password reset
// @Description  Always answers 202, whether or not the address has an account, and in the same latency envelope — the response never reveals whether an account exists (CON-161). On a hit it emails a single-use reset link; on a miss it does nothing.
// @Tags         password-reset
// @Accept       json
// @Produce      json
// @Param        body  body      passwordResetRequest  true  "Email"
// @Success      202
// @Failure      400   {object}  map[string]string
// @Failure      429   {object}  map[string]string
// @Router       /api/password-reset [post]
func (h *PasswordResetHandler) Request(c *fiber.Ctx) error {
	var req passwordResetRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	email := repository.NormalizeEmail(req.Email)

	// Rate-limit per IP and per address. A 429 leaks only that a limit was hit,
	// never whether the address has an account, so it preserves the
	// anti-enumeration property. IP first, so a throttled caller doesn't also
	// spend an address token.
	if ok, retry := h.ipLimiter.allow(c.IP()); !ok {
		return tooManyResetRequests(c, retry)
	}
	if ok, retry := h.emailLimiter.allow(email); !ok {
		return tooManyResetRequests(c, retry)
	}

	// The lookup is the only synchronous work on both the hit and miss paths, so
	// their latency is identical. On a hit the token mint + store + enqueue run
	// off the response path (dispatchReset), because an early return on a miss —
	// or extra synchronous work on a hit — would leak account existence through
	// timing alone.
	account, err := h.accountRepo.GetByEmail(reqCtx(c), email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && account != nil {
		h.dispatchReset(account)
	}
	return c.SendStatus(fiber.StatusAccepted)
}

// dispatchReset mints a single-use token, stores its hash, and enqueues the
// reset email — all off the request's response path so a hit and a miss are
// indistinguishable by latency (CON-161). It runs on a detached context (the
// fiber request context is recycled once Request returns) that still carries the
// user's tenant + id for the tenant-scoped activity event. Best-effort: a
// failure is logged, never surfaced (surfacing it would itself be an oracle).
func (h *PasswordResetHandler) dispatchReset(account *models.Account) {
	const comp = "handlers.password_reset"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// Resolve the membership the reset token attaches to (its user_id/tenant_id
		// and the activity attribution). Done here, off the response path, so a hit
		// stays latency-indistinguishable from a miss — only the account lookup is
		// synchronous. In PR1 an account has exactly one membership.
		user, err := h.userRepo.GetByAccountID(ctx, account.ID)
		if err != nil {
			slog.ErrorContext(ctx, "password reset: membership lookup failed", logging.AttrComponent, comp, logging.AttrError, err)
			return
		}

		token, hash, err := models.NewResetToken()
		if err != nil {
			slog.ErrorContext(ctx, "password reset: token gen failed", logging.AttrComponent, comp, logging.AttrError, err)
			return
		}
		id, err := models.NewID()
		if err != nil {
			slog.ErrorContext(ctx, "password reset: id gen failed", logging.AttrComponent, comp, logging.AttrError, err)
			return
		}
		now := time.Now().UTC()
		row := &models.PasswordResetToken{
			ID:        id,
			UserID:    user.ID,
			TenantID:  user.TenantID,
			TokenHash: hash,
			ExpiresAt: now.Add(models.PasswordResetTokenTTL),
			CreatedAt: now,
		}
		resetURL := strings.TrimRight(h.appBaseURL, "/") + "/auth/reset?token=" + token

		// Token row + email enqueue commit together: a rolled-back tx queues no
		// mail, a committed one queues exactly one link that resolves to a stored
		// hash.
		if err := h.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
				return err
			}
			if h.emailJobs != nil {
				return h.emailJobs.EnqueuePasswordResetTx(ctx, tx.Tx, user.ID, user.TenantID, id, resetURL)
			}
			return nil
		}); err != nil {
			slog.ErrorContext(ctx, "password reset: mint failed", logging.AttrComponent, comp, logging.AttrError, err)
			return
		}

		h.activity.Record(
			logging.WithUserID(tenantctx.With(ctx, user.TenantID), user.ID),
			activity.CategoryAuthentication, "password_reset_requested",
			activity.WithEntity("user", user.ID), activity.WithSource(activity.SourceAPI),
		)
	}()
}

type passwordResetConfirmRequest struct {
	Token string `json:"token" validate:"required"`
	// min=8 mirrors signup (createUserRequest); the client additionally requires
	// upper/lower/digit.
	Password string `json:"password" validate:"required,min=8"`
}

// Confirm godoc
// @Summary      Complete a password reset
// @Description  Consumes a single-use reset token, sets the new password, and revokes all of the user's sessions. Opens no session — the user logs in afterwards (CON-161).
// @Tags         password-reset
// @Accept       json
// @Produce      json
// @Param        body  body      passwordResetConfirmRequest  true  "Token + new password"
// @Success      204
// @Failure      400   {object}  map[string]string
// @Router       /api/password-reset/confirm [post]
func (h *PasswordResetHandler) Confirm(c *fiber.Ctx) error {
	var req passwordResetConfirmRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	// Validate the new password BEFORE touching the token, so a too-short
	// password doesn't burn a still-valid link — the user can retry it.
	if err := validate.Struct(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, validationError(err).Error())
	}

	tokenHash := models.HashResetToken(req.Token)
	now := time.Now().UTC()

	var userID, tenantID string
	err := h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		row := new(models.PasswordResetToken)
		if err := tx.NewSelect().Model(row).Where("token_hash = ?", tokenHash).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errResetInvalid
			}
			return err
		}
		// Atomic single-use consume: flip consumed_at only while the token is
		// still unspent AND unexpired. A concurrent double-submit races here and
		// exactly one UPDATE affects a row; the loser sees 0 rows and is rejected,
		// so the token can't be spent twice with two different passwords.
		res, err := tx.NewUpdate().Model((*models.PasswordResetToken)(nil)).
			Set("consumed_at = ?", now).
			Where("id = ?", row.ID).
			Where("consumed_at IS NULL").
			Where("expires_at > ?", now).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errResetInvalid // already spent or expired
		}
		userID, tenantID = row.UserID, row.TenantID

		// The token is tied to a membership, but the credential lives on the account
		// (CON-147). Resolve the account so the rotation and session revocation act
		// on the identity, not the membership row.
		membership := new(models.User)
		if err := tx.NewSelect().Model(membership).Column("account_id").
			Where("id = ?", userID).Scan(ctx); err != nil {
			return err
		}
		accountID := membership.AccountID

		// Hash the new password only once the token is confirmed live, so a bogus
		// or already-spent token is rejected cheaply — confirm is unauthenticated
		// and unthrottled, and argon2id is deliberately expensive, so hashing
		// before validating the token would let anyone force that work. Resets are
		// rare, so holding the row lock across the hash is fine.
		newHash, err := models.HashPassword(req.Password)
		if err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("password_hash = ?", newHash).
			Set("updated_at = ?", now).
			Where("id = ?", accountID).
			Exec(ctx); err != nil {
			return err
		}
		// Any other outstanding reset tokens for this user are void once one is
		// spent.
		if _, err := tx.NewUpdate().Model((*models.PasswordResetToken)(nil)).
			Set("consumed_at = ?", now).
			Where("user_id = ?", userID).
			Where("consumed_at IS NULL").
			Exec(ctx); err != nil {
			return err
		}
		// Revoke every session for the account. A reset may be locking out an
		// intruder across every workspace the account can reach, and confirm
		// deliberately opens no session (POST /api/sessions stays the only thing
		// that starts one), so there is no "current" session to preserve.
		if _, err := tx.NewDelete().Model((*models.Session)(nil)).
			Where("account_id = ?", accountID).
			Exec(ctx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errResetInvalid) {
			return fiber.NewError(fiber.StatusBadRequest, resetTokenInvalidMsg)
		}
		return err
	}

	h.activity.Record(
		logging.WithUserID(tenantctx.With(reqCtx(c), tenantID), userID),
		activity.CategoryAuthentication, "password_reset_completed",
		activity.WithEntity("user", userID), activity.WithSource(activity.SourceAPI),
	)
	return c.SendStatus(fiber.StatusNoContent)
}

// tooManyResetRequests writes the reset endpoint's 429 (CON-161) through the
// shared tooManyRequests helper (Retry-After + user-facing body).
func tooManyResetRequests(c *fiber.Ctx, retry time.Duration) error {
	return tooManyRequests(c, retry, "too many password reset requests; try again later")
}
