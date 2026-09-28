package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// CleanupEmailLogsQueue drops email_logs rows older than the retention window.
// Cadence is a River PeriodicJob; mirrors cleanup_post_logs.
const CleanupEmailLogsQueue = "cleanup_email_logs"

// CleanupEmailLogsTask is the marker payload — the worker only reads the cutoff.
type CleanupEmailLogsTask struct{}

// Kind implements river.JobArgs.
func (CleanupEmailLogsTask) Kind() string { return CleanupEmailLogsQueue }

// InsertOpts: one attempt (a failed sweep retries next tick), unique across
// active states so overlapping ticks can't stack.
func (CleanupEmailLogsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// CleanupEmailLogsProcessor deletes email_logs older than `now - retention`.
type CleanupEmailLogsProcessor struct {
	river.WorkerDefaults[CleanupEmailLogsTask]
	Repo      repository.EmailLogRepository
	Retention time.Duration
}

// Work is the River entrypoint; it delegates to Process under a system context
// (the sweep spans tenants).
func (p *CleanupEmailLogsProcessor) Work(ctx context.Context, job *river.Job[CleanupEmailLogsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *CleanupEmailLogsProcessor) Timeout(*river.Job[CleanupEmailLogsTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CleanupEmailLogsProcessor{Repo: d.Email.Logs, Retention: d.Email.Retention})
	})
}

// Process deletes email_logs entries older than the retention window.
func (p *CleanupEmailLogsProcessor) Process(ctx context.Context, _ CleanupEmailLogsTask) error {
	if p.Retention <= 0 || p.Repo == nil {
		return nil
	}
	cutoff := time.Now().UTC().Add(-p.Retention)
	n, err := p.Repo.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		return err
	}
	if n > 0 {
		slog.InfoContext(ctx, "cleanup removed email logs", logging.AttrComponent, "jobs.cleanup_email_logs", "count", n, "cutoff", cutoff.Format(time.RFC3339))
	}
	return nil
}
