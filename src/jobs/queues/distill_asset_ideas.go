package queues

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

const DistillAssetIdeasKind = "distill_asset_ideas"

// DistillRunner is the slice of the analysis use case the distill job
// drives. *assetanalysis.Service implements it.
type DistillRunner interface {
	Distill(ctx context.Context, tenantID, runID string) error
	FailDistill(ctx context.Context, tenantID, runID string, cause error)
}

// DistillRuns runs distills detached. *assetanalysis.Service implements it.
type DistillRuns interface {
	StartDistillDetached(ctx context.Context, tenantID, runID string) (string, error)
	DistillSlot(ctx context.Context, tenantID, runID string) (runslot.Slot, error)
}

// DistillAssetIdeasTask runs one distill run. The run row carries the lens,
// focus and count; a run is queued once.
type DistillAssetIdeasTask struct {
	TenantID string `json:"tenant_id" river:"unique"`
	RunID    string `json:"run_id"    river:"unique"`
}

func (DistillAssetIdeasTask) Kind() string { return DistillAssetIdeasKind }

type DistillAssetIdeasProcessor struct {
	Deps AnalysisDeps
}

// Process runs the distill in-process. On the last attempt a failure is
// stored on the run so it never stays pending.
func (p *DistillAssetIdeasProcessor) Process(ctx context.Context, task DistillAssetIdeasTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, DistillAssetIdeasKind, task, attempt, maxAttempts, localActions{})
}

// step runs the distill. With DistillRuns wired the model call runs detached
// and the job waits on it; a job restarted meanwhile (a deploy) finds the run
// in flight and waits on that one.
func (p *DistillAssetIdeasProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task DistillAssetIdeasTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	if p.Deps.Distiller == nil {
		return done("skipped_unwired")
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	if active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID); err != nil {
		return retry("distill_asset_ideas", err)
	} else if !active {
		return done("skipped_inactive")
	}
	var st analyzeState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	runs := p.Deps.DistillRuns
	if runs == nil {
		return p.settle(ctx, task, p.Deps.Distiller.Distill(ctx, task.TenantID, task.RunID), in.lastAttempt())
	}
	if st.Token == "" {
		token, err := runs.StartDistillDetached(ctx, task.TenantID, task.RunID)
		switch {
		case errors.Is(err, assetanalysis.ErrDistillGone):
			return done("skipped_run_gone")
		case err != nil:
			return retry("distill_asset_ideas", err)
		case token == "":
			// A run is already in flight, started by this job before a restart.
			slot, err := runs.DistillSlot(ctx, task.TenantID, task.RunID)
			if err != nil {
				return retry("distill_asset_ideas", err)
			}
			token = slot.Token
		}
		return waitOn(analyzeState{Token: token, Started: time.Now()})
	}
	slot, err := runs.DistillSlot(ctx, task.TenantID, task.RunID)
	switch {
	case errors.Is(err, assetanalysis.ErrDistillGone):
		return done("skipped_run_gone")
	case err != nil:
		return retry("distill_asset_ideas", err)
	case slot.Token != st.Token:
		return retry("distill_asset_ideas", errors.New("another run took the distill"))
	case slot.Done:
		return p.settle(ctx, task, slot.Err, in.lastAttempt())
	case slot.Running:
		return waitOn(st)
	default:
		// No heartbeat: ogen restarted under the run.
		return retry("distill_asset_ideas", errors.New("the distill run stopped without an outcome"))
	}
}

// settle ends the job on the distill's outcome.
func (p *DistillAssetIdeasProcessor) settle(ctx context.Context, task DistillAssetIdeasTask, runErr error, lastAttempt bool) (StepOutput, error) {
	if runErr == nil {
		return done("distilled")
	}
	if lastAttempt {
		slog.WarnContext(ctx, "distill failed on its last attempt", logging.AttrComponent, "jobs.distill_asset_ideas",
			"run_id", task.RunID, logging.AttrError, runErr)
		p.Deps.Distiller.FailDistill(ctx, task.TenantID, task.RunID, runErr)
		return done("failed")
	}
	return retry("distill_asset_ideas", runErr)
}

// EnqueueDistillIdeas queues a distill run to start now.
func (e *Enqueuer) EnqueueDistillIdeas(ctx context.Context, tenantID, runID string) error {
	return e.insert(ctx, DistillAssetIdeasTask{TenantID: tenantID, RunID: runID}, nil)
}
