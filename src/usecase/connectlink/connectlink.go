// Package connectlink issues a Zernio headless-connect link for the caller's
// tenant: it lazily bootstraps the tenant's Zernio profile, mints the
// short-lived connect session the OAuth callback resolves, requests the link,
// and marks the tenant as connect-initiated so the sync worker sweeps it.
package connectlink

import (
	"context"
	"errors"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

var (
	// ErrDegraded means the integration can't issue links right now (profile
	// bootstrap failed or the integration isn't healthy).
	ErrDegraded = errors.New("integration_degraded")
	// ErrNoTenant means the request carries no tenant.
	ErrNoTenant = errors.New("no_tenant")
)

// Service issues connect links.
type Service struct {
	Integration  *zernio.Integration
	Bootstrapper *zernio.Bootstrapper
	Settings     zernio.SettingsStore
	Sessions     repository.ZernioConnectSessionRepository
	// CallbackURL is the absolute backend OAuth callback for a session id.
	CallbackURL func(sessionID string) string
	// NewSessionID mints an unguessable connect-session id.
	NewSessionID func() (string, error)
	// SessionTTL bounds the connect session and is the link's advertised expiry.
	SessionTTL time.Duration
	// FastPollWindow is how long the sync worker polls fast after a link is
	// issued, so the user sees the connected account quickly.
	FastPollWindow time.Duration
}

// Link is an issued connect link.
type Link struct {
	ProfileID string
	URL       string
	ExpiresAt time.Time
}

// Create issues a connect link for platform. Errors are ErrDegraded,
// ErrNoTenant, a *zernio.APIError from the link request, or a store failure.
func (s *Service) Create(ctx context.Context, platform string) (*Link, error) {
	profileID, err := s.ensureProfile(ctx)
	if err != nil {
		return nil, err
	}
	// The health gate runs after the lazy bootstrap: a successful bootstrap
	// promotes a transient degraded state back to OK, so a first connect can
	// self-heal instead of being refused before it gets the chance.
	if s.Integration.State() != zernio.StateOK {
		return nil, ErrDegraded
	}

	sessionID, now, err := s.openSession(ctx, profileID, platform)
	if err != nil {
		return nil, err
	}
	url, err := s.Integration.Client.CreateConnectLink(ctx, profileID, platform, s.CallbackURL(sessionID))
	if err != nil {
		return nil, err
	}
	if tid, ok := tenantctx.From(ctx); ok {
		s.Integration.BumpFastUntil(tid, time.Now().Add(s.FastPollWindow))
	}
	if err := s.markConnectInitiated(ctx); err != nil {
		return nil, err
	}
	return &Link{ProfileID: profileID, URL: url, ExpiresAt: now.Add(s.SessionTTL)}, nil
}

// ensureProfile returns the tenant's Zernio profile id, bootstrapping it on
// the tenant's first connect (the context carries the tenant).
func (s *Service) ensureProfile(ctx context.Context) (string, error) {
	profileID, ok, err := s.Settings.Get(ctx, zernio.SettingProfileID)
	if err != nil {
		return "", err
	}
	if ok && profileID != "" {
		return profileID, nil
	}
	if err := s.Bootstrapper.Run(ctx); err != nil {
		return "", ErrDegraded
	}
	profileID, ok, err = s.Settings.Get(ctx, zernio.SettingProfileID)
	if err != nil {
		return "", err
	}
	if !ok || profileID == "" {
		return "", ErrDegraded
	}
	return profileID, nil
}

// openSession mints the connect session the headless OAuth callback resolves
// the tenant + profile from (the browser session may not survive the
// cross-site redirect through Zernio); it also holds the pending selection
// when a platform has 2+ targets. It returns the session id and its creation
// time.
func (s *Service) openSession(ctx context.Context, profileID, platform string) (string, time.Time, error) {
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return "", time.Time{}, ErrNoTenant
	}
	sessionID, err := s.NewSessionID()
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	if err := s.Sessions.Create(ctx, &models.ZernioConnectSession{
		ID:        sessionID,
		TenantID:  tenantID,
		ProfileID: profileID,
		Platform:  platform,
		Status:    models.ZernioConnectStatusPendingAuth,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(s.SessionTTL),
	}); err != nil {
		return "", time.Time{}, err
	}
	return sessionID, now, nil
}

// markConnectInitiated records, once, that the tenant started a connection.
// Every tenant has a profile from signup, so this marker — not profile
// presence — is the sync worker's only sweep selector for the tenant. The
// write must be durable: the user already holds a link and wouldn't retry, so
// a store failure is surfaced as retryable rather than silently stranding the
// account they're about to authorize.
func (s *Service) markConnectInitiated(ctx context.Context) error {
	marker, ok, err := s.Settings.Get(ctx, zernio.SettingConnectInitiatedAt)
	if err != nil {
		return err
	}
	if ok && marker != "" {
		return nil
	}
	return s.Settings.Set(ctx, zernio.SettingConnectInitiatedAt, time.Now().UTC().Format(time.RFC3339))
}
