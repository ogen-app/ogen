package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// CleanupNotificationsQueue reaps faded notifications: rows past their
// expires_at, plus read/dismissed rows older than the retention window. It
// mirrors cleanup_post_logs / cleanup_email_logs — a marker payload, self-
// registering, one attempt per tick.
const CleanupNotificationsQueue = "cleanup_notifications"

const cleanupNotificationsComp = "jobs.cleanup_notifications"

// CleanupNotificationsTask is a marker payload — this queue carries no per-tick data.
type CleanupNotificationsTask struct{}

// Kind implements river.JobArgs.
func (CleanupNotificationsTask) Kind() string { return CleanupNotificationsQueue }

// InsertOpts mirrors the other periodic sweeps: one attempt, active-state
// uniqueness so overlapping ticks don't stack.
func (CleanupNotificationsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// CleanupNotificationsProcessor reaps one tick across all tenants. A nil Repo
// (notifications unwired, e.g. in tests) makes the tick a no-op.
type CleanupNotificationsProcessor struct {
	river.WorkerDefaults[CleanupNotificationsTask]
	Repo      repository.NotificationRepository
	Retention time.Duration
}

func (p *CleanupNotificationsProcessor) Work(ctx context.Context, job *river.Job[CleanupNotificationsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	if p.Repo == nil {
		return nil
	}
	// Reaps across every tenant, so it runs under a system context (the
	// TenantScoped delete hook skips scoping under it).
	ctx = tenantctx.WithSystem(ctx)
	deleted, err := p.Repo.DeleteExpired(ctx, time.Now().UTC(), p.Retention)
	if err != nil {
		slog.ErrorContext(ctx, "notification cleanup failed", logging.AttrComponent, cleanupNotificationsComp, logging.AttrError, err)
		return err
	}
	notify.Cleaned.Add(deleted)
	if deleted > 0 {
		slog.InfoContext(ctx, "notifications cleaned", logging.AttrComponent, cleanupNotificationsComp, "deleted", deleted)
	}
	return nil
}

func (p *CleanupNotificationsProcessor) Timeout(*river.Job[CleanupNotificationsTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CleanupNotificationsProcessor{
			Repo:      d.NotificationRepo,
			Retention: d.NotificationRetention,
		})
	})
}
