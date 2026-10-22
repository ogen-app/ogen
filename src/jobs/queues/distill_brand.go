package queues

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

const (
	DistillBrandKind       = "distill_brand"
	ExpireBrandDistillKind = "expire_brand_distills"
	// distillBrandPoll is how often a run looks again at sources still being
	// ingested.
	distillBrandPoll = 15 * time.Second
)

// BrandDistillRunner is the slice of the brand distillation use case the
// jobs drive. *branddistill.Service implements it.
type BrandDistillRunner interface {
	Run(ctx context.Context, tenantID, runID string) error
	Fail(ctx context.Context, tenantID, runID string, cause error)
	FailInactive(ctx context.Context, tenantID, runID string)
	Expire(ctx context.Context) (int64, error)
}

// BrandDistillDeps are the brand distillation jobs' collaborators; a nil
// Runner makes them no-ops.
type BrandDistillDeps struct {
	Runner  BrandDistillRunner
	Tenants TenantStatusReader
}

// DistillBrandTask moves one run on. The run row carries everything; a run
// is queued once and snoozes while its sources ingest.
type DistillBrandTask struct {
	TenantID string `json:"tenant_id" river:"unique"`
	RunID    string `json:"run_id"    river:"unique"`
}

func (DistillBrandTask) Kind() string { return DistillBrandKind }

func (DistillBrandTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       AnalysisQueue,
		MaxAttempts: 5,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: analysisUniqueStates},
	}
}

type DistillBrandProcessor struct {
	river.WorkerDefaults[DistillBrandTask]
	Deps BrandDistillDeps
}

// ExpireBrandDistillsTask expires proposals nobody committed in time.
type ExpireBrandDistillsTask struct{}

func (ExpireBrandDistillsTask) Kind() string { return ExpireBrandDistillKind }

// InsertOpts: one attempt; the next tick catches up.
func (ExpireBrandDistillsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: AnalysisQueue, MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

type ExpireBrandDistillsProcessor struct {
	river.WorkerDefaults[ExpireBrandDistillsTask]
	Deps BrandDistillDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &DistillBrandProcessor{Deps: d.BrandDistill})
		river.AddWorker(w, &ExpireBrandDistillsProcessor{Deps: d.BrandDistill})
	})
}

func (p *DistillBrandProcessor) Work(ctx context.Context, job *river.Job[DistillBrandTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout covers reading ten pages or a long document and composing.
func (p *DistillBrandProcessor) Timeout(*river.Job[DistillBrandTask]) time.Duration {
	return 20 * time.Minute
}

// Process moves the run on. While its sources ingest it snoozes, which uses
// no attempt. On the last attempt a failure is stored on the run so it never
// stays open.
func (p *DistillBrandProcessor) Process(ctx context.Context, task DistillBrandTask, lastAttempt bool) error {
	if p.Deps.Runner == nil {
		return nil
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID)
	if err != nil {
		return err
	}
	if !active {
		p.Deps.Runner.FailInactive(ctx, task.TenantID, task.RunID)
		return nil
	}
	err = p.Deps.Runner.Run(ctx, task.TenantID, task.RunID)
	if errors.Is(err, branddistill.ErrWaiting) {
		return river.JobSnooze(distillBrandPoll)
	}
	if err != nil && lastAttempt {
		slog.WarnContext(ctx, "brand distillation failed on its last attempt", logging.AttrComponent, "jobs.distill_brand",
			"run_id", task.RunID, logging.AttrError, err)
		p.Deps.Runner.Fail(ctx, task.TenantID, task.RunID, err)
		return nil
	}
	return err
}

func (p *ExpireBrandDistillsProcessor) Work(ctx context.Context, _ *river.Job[ExpireBrandDistillsTask]) error {
	if p.Deps.Runner == nil {
		return nil
	}
	n, err := p.Deps.Runner.Expire(ctx)
	if n > 0 {
		slog.InfoContext(ctx, "expired brand proposals", logging.AttrComponent, "jobs.expire_brand_distills", "expired", n)
	}
	return err
}

// EnqueueDistillBrand queues a run to start now.
func (e *Enqueuer) EnqueueDistillBrand(ctx context.Context, tenantID, runID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.Insert(ctx, DistillBrandTask{TenantID: tenantID, RunID: runID}, insertOptsWithRequestID(ctx, nil))
	return err
}
