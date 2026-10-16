package queues

import (
	"context"
	"log/slog"
	"time"

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

// Kind implements JobArgs.
func (CleanupNotificationsTask) Kind() string { return CleanupNotificationsQueue }

// CleanupNotificationsProcessor reaps one tick across all tenants. A nil Repo
// (notifications unwired, e.g. in tests) makes the tick a no-op.
type CleanupNotificationsProcessor struct {
	Repo      repository.NotificationRepository
	Retention time.Duration
}

// Process reaps expired and old notifications across every tenant.
func (p *CleanupNotificationsProcessor) Process(ctx context.Context, _ CleanupNotificationsTask) error {
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

func newCleanupNotifications(d Deps) *CleanupNotificationsProcessor {
	return &CleanupNotificationsProcessor{Repo: d.NotificationRepo, Retention: d.NotificationRetention}
}
