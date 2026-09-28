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

// CleanupPostLogsQueue is the recurring sweep that drops Post Log entries
// older than the configured retention window. Its cadence is a
// River PeriodicJob (see PeriodicConfig); the marker is unique across active
// states so overlapping ticks can't stack.
const CleanupPostLogsQueue = "cleanup_post_logs"

// CleanupPostLogsTask is the typed payload for the queue. There's
// nothing to carry — the worker just looks at the cutoff window — but
// River still requires a per-kind args type so we satisfy JobArgs with
// a marker struct.
type CleanupPostLogsTask struct{}

// Kind implements river.JobArgs.
func (CleanupPostLogsTask) Kind() string { return CleanupPostLogsQueue }

// InsertOpts sets per-kind defaults. Cadence is owned by a River PeriodicJob,
// so a single attempt is enough — a failed sweep retries on the next tick.
// UniqueOpts (active states only) prevents overlapping ticks from stacking.
func (CleanupPostLogsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// CleanupPostLogsProcessor is the River worker for cleanup_post_logs. The
// handler deletes Post Logs older than `now - retention`. Cadence is owned by
// a River PeriodicJob.
type CleanupPostLogsProcessor struct {
	river.WorkerDefaults[CleanupPostLogsTask]
	Repo      repository.PostLogRepository
	Retention time.Duration
}

// Work is the River entrypoint; it delegates to Process.
func (p *CleanupPostLogsProcessor) Work(ctx context.Context, job *river.Job[CleanupPostLogsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *CleanupPostLogsProcessor) Timeout(*river.Job[CleanupPostLogsTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CleanupPostLogsProcessor{Repo: d.Zernio.PostLogRepo, Retention: d.PostLogRetention})
	})
}

// Process deletes Post Log entries older than the retention window.
func (p *CleanupPostLogsProcessor) Process(ctx context.Context, _ CleanupPostLogsTask) error {
	if p.Retention > 0 && p.Repo != nil {
		cutoff := time.Now().UTC().Add(-p.Retention)
		n, err := p.Repo.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			return err
		}
		if n > 0 {
			jobs.PostLogCleaned.Add(n)
			slog.InfoContext(ctx, "cleanup removed entries", logging.AttrComponent, "jobs.cleanup_post_logs", "count", n, "cutoff", cutoff.Format(time.RFC3339))
		}
	}
	return nil
}
