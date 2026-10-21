package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const DistillAssetIdeasKind = "distill_asset_ideas"

// DistillRunner is the slice of the analysis use case the distill job
// drives. *assetanalysis.Service implements it.
type DistillRunner interface {
	Distill(ctx context.Context, tenantID, runID string) error
	FailDistill(ctx context.Context, tenantID, runID string, cause error)
}

// DistillAssetIdeasTask runs one distill run. The run row carries the lens,
// focus and count; a run is queued once.
type DistillAssetIdeasTask struct {
	TenantID string `json:"tenant_id" river:"unique"`
	RunID    string `json:"run_id"    river:"unique"`
}

func (DistillAssetIdeasTask) Kind() string { return DistillAssetIdeasKind }

func (DistillAssetIdeasTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       AnalysisQueue,
		MaxAttempts: 5,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: analysisUniqueStates},
	}
}

type DistillAssetIdeasProcessor struct {
	river.WorkerDefaults[DistillAssetIdeasTask]
	Deps AnalysisDeps
}

func (p *DistillAssetIdeasProcessor) Work(ctx context.Context, job *river.Job[DistillAssetIdeasTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout covers one call over a source near the full-context cap.
func (p *DistillAssetIdeasProcessor) Timeout(*river.Job[DistillAssetIdeasTask]) time.Duration {
	return 10 * time.Minute
}

// Process runs the distill. On the last attempt a failure is stored on the
// run so it never stays pending.
func (p *DistillAssetIdeasProcessor) Process(ctx context.Context, task DistillAssetIdeasTask, lastAttempt bool) error {
	if p.Deps.Distiller == nil {
		return nil
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	if active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID); err != nil || !active {
		return err
	}
	err := p.Deps.Distiller.Distill(ctx, task.TenantID, task.RunID)
	if err != nil && lastAttempt {
		slog.WarnContext(ctx, "distill failed on its last attempt", logging.AttrComponent, "jobs.distill_asset_ideas",
			"run_id", task.RunID, logging.AttrError, err)
		p.Deps.Distiller.FailDistill(ctx, task.TenantID, task.RunID, err)
		return nil
	}
	return err
}

// EnqueueDistillIdeas queues a distill run to start now.
func (e *Enqueuer) EnqueueDistillIdeas(ctx context.Context, tenantID, runID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.Insert(ctx, DistillAssetIdeasTask{TenantID: tenantID, RunID: runID}, insertOptsWithRequestID(ctx, nil))
	return err
}
