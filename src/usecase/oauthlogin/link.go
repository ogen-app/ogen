package oauthlogin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// linkExisting links a verified identity to the account its address matches
// and signs that account in.
//
// Ogen never verified the addresses typed into a password signup, so the
// existing password may belong to someone who registered the address before
// its owner arrived. The provider has now proven the owner controls the
// mailbox, so linking removes that password, voids pending reset links and
// signs every other session out; the owner can set a new password through
// "Forgot password". All of it happens under the account row lock, so a
// concurrent password login (which checks the hash under the same lock) either
// finishes first and is signed out, or sees no password.
func (s *Service) linkExisting(ctx context.Context, account *models.Account, c *Claims) (*Result, error) {
	linked, err := s.d.Identities.ListForAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(linked, func(i models.AccountIdentity) bool { return i.Provider == s.d.Provider.Name() }) {
		// The account is connected to a different Google account already.
		return nil, &Error{Code: CodeAlreadyConnected}
	}

	user, err := s.d.Users.GetByAccountID(ctx, account.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	identity, err := s.newIdentity(account.ID, c)
	if err != nil {
		return nil, err
	}
	var session *models.Session
	if user != nil {
		if session, err = s.newSession(account.ID, user); err != nil {
			return nil, err
		}
	}

	var hadPassword bool
	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if hadPassword, err = s.lockAndClearPassword(ctx, tx, account.ID); err != nil {
			return err
		}
		if err := s.d.Identities.Create(ctx, tx, identity); err != nil {
			return err
		}
		if _, err := s.d.Sessions.DeleteAllForAccount(ctx, tx, account.ID, ""); err != nil {
			return err
		}
		if session == nil {
			return nil
		}
		if _, err := tx.NewInsert().Model(session).Exec(ctx); err != nil {
			return err
		}
		if s.d.Emails == nil {
			return nil
		}
		return s.d.Emails.EnqueueGoogleLinkedEmailTx(ctx, tx.Tx, user.ID, user.TenantID, identity.ID, c.Email, hadPassword)
	})
	if isUniqueViolation(err) {
		// A concurrent callback linked the same identity or account first.
		return nil, &Error{Code: CodeStateInvalid, Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("oauthlogin: link identity: %w", err)
	}
	if user == nil {
		return nil, &Error{Code: CodeNoActiveWorkspace}
	}
	account.PasswordHash = ""
	return &Result{Session: session, Account: account, User: user, Linked: true, PasswordCleared: hadPassword}, nil
}

// lockAndClearPassword takes the account row lock, removes its password and
// voids its outstanding reset links. It reports whether there was a password.
func (s *Service) lockAndClearPassword(ctx context.Context, tx bun.Tx, accountID string) (bool, error) {
	var hasPassword bool
	if err := tx.NewSelect().Model((*models.Account)(nil)).ColumnExpr("password_hash IS NOT NULL").
		Where("id = ?", accountID).For("UPDATE").Scan(ctx, &hasPassword); err != nil {
		return false, err
	}
	if err := s.d.Accounts.ClearPasswordTx(ctx, tx, accountID); err != nil {
		return false, err
	}
	_, err := tx.NewUpdate().Model((*models.PasswordResetToken)(nil)).
		Set("consumed_at = ?", s.clock()).
		Where("consumed_at IS NULL").
		Where("user_id IN (SELECT id FROM users WHERE account_id = ?)", accountID).
		Exec(ctx)
	return hasPassword, err
}

func (s *Service) newIdentity(accountID string, c *Claims) (*models.AccountIdentity, error) {
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := s.clock()
	return &models.AccountIdentity{
		ID: id, AccountID: accountID, Provider: s.d.Provider.Name(), Subject: c.Subject,
		Email: c.Email, LinkedAt: now, LastUsedAt: &now,
	}, nil
}

// newSession builds a session on the given membership.
func (s *Service) newSession(accountID string, user *models.User) (*models.Session, error) {
	token, err := models.NewSessionToken()
	if err != nil {
		return nil, err
	}
	now := s.clock()
	return &models.Session{
		ID:        token,
		AccountID: accountID,
		UserID:    user.ID,
		TenantID:  user.TenantID,
		ExpiresAt: now.Add(sessionTTL),
		CreatedAt: now,
	}, nil
}

func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}
