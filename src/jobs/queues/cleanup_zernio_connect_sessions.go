package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// CleanupZernioConnectSessionsQueue is the recurring sweep that drops expired
// headless-connect sessions. Correctness never depends on it —
// readers already treat expires_at in the past as gone — it just reclaims rows.
// river-service schedules it; ogen runs each pass (MaintenanceService).
const CleanupZernioConnectSessionsQueue = "cleanup_zernio_connect_sessions"

// CleanupZernioConnectSessionsTask is the marker payload — the worker just
// deletes by the expiry cutoff, so there's nothing to carry.
type CleanupZernioConnectSessionsTask struct{}

// Kind implements JobArgs.
func (CleanupZernioConnectSessionsTask) Kind() string { return CleanupZernioConnectSessionsQueue }

// CleanupZernioConnectSessionsProcessor runs the sweep.
type CleanupZernioConnectSessionsProcessor struct {
	Repo repository.ZernioConnectSessionRepository
}

func newCleanupZernioConnectSessions(d Deps) *CleanupZernioConnectSessionsProcessor {
	return &CleanupZernioConnectSessionsProcessor{Repo: d.ConnectSessionRepo}
}

// Process deletes connect sessions whose expiry has passed.
func (p *CleanupZernioConnectSessionsProcessor) Process(ctx context.Context, _ CleanupZernioConnectSessionsTask) error {
	if p.Repo == nil {
		return nil
	}
	n, err := p.Repo.DeleteExpired(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		jobs.ZernioConnectSessionsSwept.Add(n)
		slog.InfoContext(ctx, "swept expired connect sessions",
			logging.AttrComponent, "jobs.cleanup_zernio_connect_sessions", "count", n)
	}
	return nil
}
