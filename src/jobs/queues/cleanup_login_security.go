package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
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

// Kind implements river.JobArgs.
func (CleanupLoginSecurityTask) Kind() string { return CleanupLoginSecurityQueue }

// InsertOpts sets per-kind defaults. A failed sweep retries on the next tick;
// active-state UniqueOpts prevents overlapping ticks from stacking.
func (CleanupLoginSecurityTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// CleanupLoginSecurityProcessor is the River worker for the sweep.
type CleanupLoginSecurityProcessor struct {
	river.WorkerDefaults[CleanupLoginSecurityTask]
	Devices  repository.KnownDeviceRepository
	Alerts   repository.LoginAlertTokenRepository
	Sessions repository.SessionRepository
}

// Work is the River entrypoint; it delegates to Process.
func (p *CleanupLoginSecurityProcessor) Work(ctx context.Context, job *river.Job[CleanupLoginSecurityTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Both tables are account-level, not tenant-scoped.
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *CleanupLoginSecurityProcessor) Timeout(*river.Job[CleanupLoginSecurityTask]) time.Duration {
	return time.Minute
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CleanupLoginSecurityProcessor{Devices: d.KnownDeviceRepo, Alerts: d.LoginAlertTokenRepo, Sessions: d.SessionRepo})
	})
}

// Process deletes alert tokens older than the retention window, devices
// unseen for longer than the device cookie lives (such a browser has lost the
// cookie, so its row can never match again), and expired sessions.
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
	return nil
}

func logSwept(ctx context.Context, what string, n int) {
	if n > 0 {
		slog.InfoContext(ctx, "swept expired "+what,
			logging.AttrComponent, "jobs.cleanup_login_security", "count", n)
	}
}
