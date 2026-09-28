package loginsecurity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

// Errors for an alert token that can't be previewed or used.
var (
	ErrNotFound = errors.New("login alert not found")
	ErrUsed     = errors.New("login alert already used")
	ErrExpired  = errors.New("login alert expired")
)

// Preview is what the secure-account page shows before the owner confirms.
type Preview struct {
	Status   string    `json:"status"`
	LoginAt  time.Time `json:"login_at"`
	Device   string    `json:"device"`
	IP       string    `json:"ip"`
	Location string    `json:"location"`
	Email    string    `json:"email"`
}

// Preview reports the sign-in an alert link refers to and whether the link can
// still be used. It changes nothing: mail scanners follow links with GET.
func (s *Service) Preview(ctx context.Context, rawToken string) (*Preview, error) {
	if !models.ValidLoginAlertToken(rawToken) {
		return nil, ErrNotFound
	}
	tok, err := s.d.Alerts.GetByHash(ctx, models.HashLoginAlertToken(rawToken))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p := &Preview{
		Status:   tok.Status(s.clock()),
		LoginAt:  tok.LoginAt.UTC(),
		Device:   tok.DeviceLabel,
		IP:       tok.IP,
		Location: tok.Location,
	}
	if acc, err := s.d.Accounts.GetByID(ctx, tok.AccountID); err == nil {
		p.Email = MaskEmail(acc.Email)
	}
	return p, nil
}

// Secured is the outcome of securing an account.
type Secured struct {
	// ResetURL is a fresh single-use password-reset link. It is empty when the
	// account has no live workspace to attach a reset token to; such an account
	// can't sign in anyway.
	ResetURL        string `json:"reset_url"`
	SessionsRevoked int    `json:"sessions_revoked"`
}

// Secure spends an alert token and, in the same transaction, signs the account
// out everywhere, forgets its devices, voids its other alert links and mints a
// password-reset token. It sends no email and opens no session: the owner is
// already in the flow, and only a password login starts a session.
func (s *Service) Secure(ctx context.Context, rawToken string) (*Secured, error) {
	if !models.ValidLoginAlertToken(rawToken) {
		return nil, ErrNotFound
	}
	hash := models.HashLoginAlertToken(rawToken)
	now := s.clock()

	var (
		res     Secured
		tok     *models.LoginAlertToken
		devices int
	)
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if tok, err = s.d.Alerts.Consume(ctx, tx, hash, now); err != nil {
			return err
		}
		if res.SessionsRevoked, err = s.d.Sessions.DeleteAllForAccount(ctx, tx, tok.AccountID, ""); err != nil {
			return err
		}
		if devices, err = s.d.Devices.DeleteForAccount(ctx, tx, tok.AccountID); err != nil {
			return err
		}
		if _, err = s.d.Alerts.VoidPending(ctx, tx, tok.AccountID, now); err != nil {
			return err
		}
		res.ResetURL, err = s.mintReset(ctx, tx, tok.AccountID, now)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) && tok == nil {
		return nil, s.unusable(ctx, hash)
	}
	if err != nil {
		return nil, err
	}

	metricAccountsSecured.Add(1)
	s.d.Activity.Record(actorCtx(ctx, tok.TenantID, tok.UserID), activity.CategoryAuthentication, "account_secured",
		activity.WithEntity("user", tok.UserID),
		activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{
			"sessions_revoked": res.SessionsRevoked,
			"devices_cleared":  devices,
			"token_id":         tok.ID,
		}),
	)
	return &res, nil
}

// mintReset issues a password-reset token for the account's current default
// membership (reset tokens belong to a membership), returning its link. The
// membership the alert was raised for may have been removed since.
func (s *Service) mintReset(ctx context.Context, tx bun.Tx, accountID string, now time.Time) (string, error) {
	user, err := s.d.Users.GetByAccountID(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	raw, hash, err := models.NewResetToken()
	if err != nil {
		return "", err
	}
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	row := &models.PasswordResetToken{
		ID: id, UserID: user.ID, TenantID: user.TenantID, TokenHash: hash,
		ExpiresAt: now.Add(models.PasswordResetTokenTTL), CreatedAt: now,
	}
	if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
		return "", err
	}
	return strings.TrimRight(s.d.AppBaseURL, "/") + "/auth/reset?token=" + raw, nil
}

// unusable explains why a token could not be consumed.
func (s *Service) unusable(ctx context.Context, hash string) error {
	tok, err := s.d.Alerts.GetByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if tok.Status(s.clock()) == models.LoginAlertUsed {
		return ErrUsed
	}
	return ErrExpired
}
