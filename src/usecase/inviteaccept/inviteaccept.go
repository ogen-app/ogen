// Package inviteaccept joins an invitee to the workspace an invitation names.
// It is shared by the password accept endpoint and the Google sign-in flow, so
// both spend the single-use token, check the team-seat quota and create the
// membership the same way.
package inviteaccept

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// sessionTTL matches a password login's session lifetime.
const sessionTTL = 7 * 24 * time.Hour

var (
	// ErrInvalid covers every unusable token: unknown, expired, revoked or
	// already accepted. They read the same so accepting is not an oracle.
	ErrInvalid = errors.New("invitation invalid")
	// ErrAlreadyMember: the account already belongs to the workspace.
	ErrAlreadyMember = errors.New("already a member of this workspace")
	// ErrAccountExists: the address gained an account between the caller's
	// check and the insert.
	ErrAccountExists = errors.New("email already has an account")
)

// Service joins invitees to workspaces.
type Service struct {
	db       *bun.DB
	users    repository.UserRepository
	accounts repository.AccountRepository
	invites  repository.InvitationRepository
	sessions repository.SessionRepository
	limiter  *entitlements.Limiter // nil-safe
}

// New returns a Service. limiter may be nil (no seat quota).
func New(db *bun.DB, users repository.UserRepository, accounts repository.AccountRepository, invites repository.InvitationRepository, sessions repository.SessionRepository, limiter *entitlements.Limiter) *Service {
	return &Service{db: db, users: users, accounts: accounts, invites: invites, sessions: sessions, limiter: limiter}
}

// Pending returns the invitation behind a token if it can still be accepted,
// without spending it, or ErrInvalid.
func (s *Service) Pending(ctx context.Context, tokenHash string) (*models.Invitation, error) {
	inv, err := s.invites.GetByTokenHash(ctx, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	if inv.Status != models.InvitationPending || time.Now().UTC().After(inv.ExpiresAt) {
		return nil, ErrInvalid
	}
	return inv, nil
}

// JoinExisting adds a membership of the invite's workspace to an account that
// already exists. The caller has established that the person is that account.
// The seat quota is checked before the token is spent, so an over-cap
// workspace leaves the invite usable once a seat frees up; the quota error
// (*entitlements.QuotaExceededError) is returned as is.
func (s *Service) JoinExisting(ctx context.Context, inv *models.Invitation, account *models.Account) (*models.User, error) {
	if _, err := s.users.GetMembership(ctx, account.ID, inv.TenantID); err == nil {
		return nil, ErrAlreadyMember
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	seats, err := s.seatQuota(ctx, inv.TenantID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	uid, err := models.NewID()
	if err != nil {
		return nil, err
	}
	membership := &models.User{ID: uid, AccountID: account.ID, TenantID: inv.TenantID, Name: account.Name, Email: account.Email, Role: inv.Role, CreatedAt: now, UpdatedAt: now}
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.consume(ctx, tx, inv, now); err != nil {
			return err
		}
		return s.users.CreateTx(ctx, tx, membership)
	})
	if isUniqueViolation(err) {
		return nil, ErrAlreadyMember
	}
	if err != nil {
		return nil, err
	}
	s.limiter.DispatchCrossing(tenantctx.With(ctx, inv.TenantID), inv.TenantID, seats)
	return membership, nil
}

// NewAccount is how a brand-new invitee will sign in: a password (its hash)
// or a verified provider identity, which leaves the account without a password.
type NewAccount struct {
	Name         string
	PasswordHash string
	Identity     *models.AccountIdentity
}

// JoinNew creates the invitee's account, its membership of the invite's
// workspace and a session on it, in one transaction with spending the token.
// It returns ErrAccountExists when the address gained an account meanwhile;
// the invite is then left unspent.
func (s *Service) JoinNew(ctx context.Context, inv *models.Invitation, na NewAccount) (*models.User, *models.Session, error) {
	seats, err := s.seatQuota(ctx, inv.TenantID)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	ids := make([]string, 2)
	for i := range ids {
		if ids[i], err = models.NewID(); err != nil {
			return nil, nil, err
		}
	}
	accountID, userID := ids[0], ids[1]
	token, err := models.NewSessionToken()
	if err != nil {
		return nil, nil, err
	}
	account := &models.Account{ID: accountID, Email: inv.Email, PasswordHash: na.PasswordHash, Name: na.Name, CreatedAt: now, UpdatedAt: now}
	user := &models.User{ID: userID, AccountID: accountID, TenantID: inv.TenantID, Name: na.Name, Email: inv.Email, Role: inv.Role, CreatedAt: now, UpdatedAt: now}
	session := &models.Session{ID: token, AccountID: accountID, UserID: userID, TenantID: inv.TenantID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now}

	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.consume(ctx, tx, inv, now); err != nil {
			return err
		}
		// accounts.email is the uniqueness backstop against a raced signup.
		if err := s.accounts.CreateTx(ctx, tx, account); err != nil {
			return err
		}
		if na.Identity != nil {
			na.Identity.AccountID = accountID
			if _, err := tx.NewInsert().Model(na.Identity).Exec(ctx); err != nil {
				return err
			}
		}
		if err := s.users.CreateTx(ctx, tx, user); err != nil {
			return err
		}
		return s.sessions.CreateTx(ctx, tx, session)
	})
	if isUniqueViolation(err) {
		return nil, nil, ErrAccountExists
	}
	if err != nil {
		return nil, nil, err
	}
	s.limiter.DispatchCrossing(tenantctx.With(ctx, inv.TenantID), inv.TenantID, seats)
	return user, session, nil
}

// consume spends the token inside the joining transaction.
func (s *Service) consume(ctx context.Context, tx bun.Tx, inv *models.Invitation, now time.Time) error {
	if _, err := s.invites.ConsumeByTokenTx(ctx, tx, inv.TokenHash, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalid
		}
		return err
	}
	return nil
}

// seatQuota checks team_seats for the workspace being joined. The invitee is
// unauthenticated or signed in elsewhere, so the count is scoped to the
// invite's tenant explicitly.
func (s *Service) seatQuota(ctx context.Context, tenantID string) (entitlements.Decision, error) {
	return s.limiter.Require(tenantctx.With(ctx, tenantID), tenantID, "team_seats")
}

func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}
