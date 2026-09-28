package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// CleanupZernioConnectSessionsQueue is the recurring sweep that drops expired
// headless-connect sessions. Correctness never depends on it —
// readers already treat expires_at in the past as gone — it just reclaims rows.
// Cadence is a River PeriodicJob (see PeriodicConfig); the marker is unique
// across active states so overlapping ticks can't stack.
const CleanupZernioConnectSessionsQueue = "cleanup_zernio_connect_sessions"

// CleanupZernioConnectSessionsTask is the marker payload — the worker just
// deletes by the expiry cutoff, so there's nothing to carry.
type CleanupZernioConnectSessionsTask struct{}

// Kind implements river.JobArgs.
func (CleanupZernioConnectSessionsTask) Kind() string { return CleanupZernioConnectSessionsQueue }

// InsertOpts sets per-kind defaults. Cadence is owned by a River PeriodicJob, so
// a single attempt is enough — a failed sweep retries on the next tick. Active-
// state UniqueOpts prevents overlapping ticks from stacking.
func (CleanupZernioConnectSessionsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// CleanupZernioConnectSessionsProcessor is the River worker for the sweep.
type CleanupZernioConnectSessionsProcessor struct {
	river.WorkerDefaults[CleanupZernioConnectSessionsTask]
	Repo repository.ZernioConnectSessionRepository
}

// Work is the River entrypoint; it delegates to Process.
func (p *CleanupZernioConnectSessionsProcessor) Work(ctx context.Context, job *river.Job[CleanupZernioConnectSessionsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// The sweep spans tenants (the table is not tenant-scoped); a system context
	// lets the delete run unscoped.
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *CleanupZernioConnectSessionsProcessor) Timeout(*river.Job[CleanupZernioConnectSessionsTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CleanupZernioConnectSessionsProcessor{Repo: d.ConnectSessionRepo})
	})
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
