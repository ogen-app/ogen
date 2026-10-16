package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// CleanupLoginSecurityQueue is the daily retention sweep for login-security
// data. Alert tokens and known devices hold IP addresses and locations, which
// are personal data, so they are kept no longer than they are useful.
const CleanupLoginSecurityQueue = "cleanup_login_security"

// loginAlertTokenRetention keeps alert tokens well past their 24h lifetime, so
// a late click still reads "used" or "expired" rather than "not found".
const loginAlertTokenRetention = 30 * 24 * time.Hour

// CleanupLoginSecurityTask is the marker payload; the worker deletes by age.
type CleanupLoginSecurityTask struct{}

// Kind implements JobArgs.
func (CleanupLoginSecurityTask) Kind() string { return CleanupLoginSecurityQueue }

// CleanupLoginSecurityProcessor runs the sweep.
type CleanupLoginSecurityProcessor struct {
	Devices  repository.KnownDeviceRepository
	Alerts   repository.LoginAlertTokenRepository
	Sessions repository.SessionRepository
	OAuth    repository.OAuthRepository
}

func newCleanupLoginSecurity(d Deps) *CleanupLoginSecurityProcessor {
	return &CleanupLoginSecurityProcessor{Devices: d.KnownDeviceRepo, Alerts: d.LoginAlertTokenRepo, Sessions: d.SessionRepo, OAuth: d.OAuthRepo}
}

// Process deletes alert tokens older than the retention window, devices
// unseen for longer than the device cookie lives (such a browser has lost the
// cookie, so its row can never match again), expired sessions, and expired
// provider sign-in states and pending signups.
func (p *CleanupLoginSecurityProcessor) Process(ctx context.Context, _ CleanupLoginSecurityTask) error {
	now := time.Now().UTC()
	if p.Alerts != nil {
		n, err := p.Alerts.DeleteCreatedBefore(ctx, now.Add(-loginAlertTokenRetention))
		if err != nil {
			return err
		}
		jobs.LoginAlertTokensSwept.Add(int64(n))
		logSwept(ctx, "login alert tokens", n)
	}
	if p.Devices != nil {
		n, err := p.Devices.DeleteUnseenSince(ctx, now.Add(-models.KnownDeviceCookieMaxAge))
		if err != nil {
			return err
		}
		jobs.KnownDevicesSwept.Add(int64(n))
		logSwept(ctx, "known devices", n)
	}
	if p.Sessions != nil {
		n, err := p.Sessions.DeleteExpiredBefore(ctx, now)
		if err != nil {
			return err
		}
		jobs.ExpiredSessionsSwept.Add(int64(n))
		logSwept(ctx, "sessions", n)
	}
	if p.OAuth != nil {
		n, err := p.OAuth.DeleteExpiredBefore(ctx, now)
		if err != nil {
			return err
		}
		jobs.OAuthRowsSwept.Add(int64(n))
		logSwept(ctx, "oauth sign-in rows", n)
	}
	return nil
}

func logSwept(ctx context.Context, what string, n int) {
	if n > 0 {
		slog.InfoContext(ctx, "swept expired "+what,
			logging.AttrComponent, "jobs.cleanup_login_security", "count", n)
	}
}
