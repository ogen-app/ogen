package oauthlogin

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// ErrPendingNotFound reports a pending signup token that is unknown, already
// used or expired. All three read the same so the token is no oracle.
var ErrPendingNotFound = errors.New("oauthlogin: pending signup not found")

// pendingSignup holds a verified newcomer until they name their workspace.
func (s *Service) pendingSignup(ctx context.Context, st *models.OAuthState, c *Claims) (*Result, error) {
	token, hash, err := models.NewOAuthToken()
	if err != nil {
		return nil, err
	}
	now := s.clock()
	if err := s.d.OAuth.CreatePendingSignup(ctx, &models.OAuthPendingSignup{
		TokenHash: hash,
		Provider:  s.d.Provider.Name(),
		Subject:   c.Subject,
		Email:     c.Email,
		Name:      c.Name,
		ReturnTo:  st.ReturnTo,
		ExpiresAt: now.Add(models.OAuthPendingSignupTTL),
		CreatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("oauthlogin: store pending signup: %w", err)
	}
	return &Result{Kind: ResultPendingSignup, PendingToken: token}, nil
}

// PendingPreview is what the "name your workspace" step shows.
type PendingPreview struct {
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PendingSignup previews a pending signup without spending it.
func (s *Service) PendingSignup(ctx context.Context, token string) (*PendingPreview, error) {
	if !models.ValidOAuthToken(token) {
		return nil, ErrPendingNotFound
	}
	p, err := s.d.OAuth.GetPendingSignup(ctx, models.HashOAuthToken(token), s.clock())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPendingNotFound
	}
	if err != nil {
		return nil, err
	}
	return &PendingPreview{Email: p.Email, Name: p.Name, ExpiresAt: p.ExpiresAt.UTC()}, nil
}

// Completed is a finished provider signup.
type Completed struct {
	*signup.Result
	ReturnTo string
}

// CompleteSignup spends a pending signup and creates the workspace, the
// passwordless account and its identity. The token is spent first, so a
// double-submitted form creates one workspace; a signup that then fails (the
// address or identity was taken meanwhile) sends the user back through the
// provider, where sign-in now finds the account. Returns ErrPendingNotFound,
// signup.ErrEmailInUse or signup.ErrIdentityLinked.
func (s *Service) CompleteSignup(ctx context.Context, token, tenantName string) (*Completed, error) {
	if !models.ValidOAuthToken(token) {
		return nil, ErrPendingNotFound
	}
	p, err := s.d.OAuth.ConsumePendingSignup(ctx, nil, models.HashOAuthToken(token), s.clock())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPendingNotFound
	}
	if err != nil {
		return nil, err
	}
	res, err := s.d.Signup.Create(ctx, signup.Input{
		TenantName: tenantName,
		UserName:   displayName(p.Name, p.Email),
		Email:      p.Email,
		Identity:   &signup.Identity{Provider: p.Provider, Subject: p.Subject, Email: p.Email},
	})
	if err != nil {
		return nil, err
	}
	return &Completed{Result: res, ReturnTo: p.ReturnTo}, nil
}

// displayName falls back to the address's local part when the provider shared
// no name.
func displayName(name, email string) string {
	local, _, _ := strings.Cut(email, "@")
	return cmp.Or(strings.TrimSpace(name), local)
}
