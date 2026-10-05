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

// CleanupPluginPairingsQueue is the recurring sweep that drops expired plugin
// pairings. Correctness never depends on it — readers treat expires_at in the
// past as gone — it only reclaims rows, including any sealed token an
// approved pairing was never collected with.
const CleanupPluginPairingsQueue = "cleanup_plugin_pairings"

// CleanupPluginPairingsTask is the marker payload.
type CleanupPluginPairingsTask struct{}

// Kind implements river.JobArgs.
func (CleanupPluginPairingsTask) Kind() string { return CleanupPluginPairingsQueue }

// InsertOpts: cadence is owned by a River PeriodicJob, so one attempt is
// enough; active-state uniqueness keeps overlapping ticks from stacking.
func (CleanupPluginPairingsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// CleanupPluginPairingsProcessor is the River worker for the sweep.
type CleanupPluginPairingsProcessor struct {
	river.WorkerDefaults[CleanupPluginPairingsTask]
	Repo repository.PluginPairingRepository
}

// Work is the River entrypoint. Pairings aren't tenant-scoped, so the delete
// runs in a system context.
func (p *CleanupPluginPairingsProcessor) Work(ctx context.Context, job *river.Job[CleanupPluginPairingsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(tenantctx.WithSystem(ctx), job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *CleanupPluginPairingsProcessor) Timeout(*river.Job[CleanupPluginPairingsTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CleanupPluginPairingsProcessor{Repo: d.PluginPairingRepo})
	})
}

// Process deletes pairings whose expiry has passed. A nil repo is a no-op.
func (p *CleanupPluginPairingsProcessor) Process(ctx context.Context, _ CleanupPluginPairingsTask) error {
	if p.Repo == nil {
		return nil
	}
	n, err := p.Repo.DeleteExpired(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		jobs.PluginPairingsSwept.Add(n)
		slog.InfoContext(ctx, "swept expired plugin pairings",
			logging.AttrComponent, "jobs.cleanup_plugin_pairings", "count", n)
	}
	return nil
}
