package queues

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
	"github.com/ogen-app/ogen/src/usecase/runslot"
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
	Prepare(ctx context.Context, tenantID, runID string, mapper branddistill.Mapper) error
	Finish(ctx context.Context, tenantID, runID string) error
	Fail(ctx context.Context, tenantID, runID string, cause error)
	FailInactive(ctx context.Context, tenantID, runID string)
	Expire(ctx context.Context) (int64, error)
}

// BrandDistillRuns runs a run's model stages detached and lists a site's
// pages. *branddistill.Service implements it.
type BrandDistillRuns interface {
	StartFinishDetached(ctx context.Context, tenantID, runID string) (string, error)
	FinishSlot(ctx context.Context, tenantID, runID string) (runslot.Slot, error)
	Map(ctx context.Context, in firecrawl.MapRequest) ([]firecrawl.Link, error)
	CanMap() bool
}

// BrandDistillDeps are the brand distillation jobs' collaborators; a nil
// Runner makes them no-ops.
type BrandDistillDeps struct {
	Runner BrandDistillRunner
	// Runs runs the model stages detached and maps sites through
	// river-service; nil runs both inline.
	Runs    BrandDistillRuns
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
	return runLocal(ctx, p.step, job.Kind, job.Args, job.Attempt, job.MaxAttempts, localActions{})
}

// Timeout covers reading ten pages or a long document and composing.
func (p *DistillBrandProcessor) Timeout(*river.Job[DistillBrandTask]) time.Duration {
	return 20 * time.Minute
}

// Process moves the run on in-process. On the last attempt a failure is
// stored on the run so it never stays open.
func (p *DistillBrandProcessor) Process(ctx context.Context, task DistillBrandTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, DistillBrandKind, task, attempt, maxAttempts, localActions{})
}

// brandState is where a distill_brand job is: waiting on the site map it
// asked for, or on its detached model stages.
type brandState struct {
	Map     *firecrawl.MapRequest `json:"map,omitempty"`
	Token   string                `json:"token,omitempty"`
	Started time.Time             `json:"started,omitzero"`
}

// step moves the run on. The quick work (admit, website, sources) runs in
// the step; the site map is made by river-service; while sources ingest the
// job snoozes, which uses no attempt. The model stages run detached and the
// job waits on them, so a deploy never cuts a model call.
func (p *DistillBrandProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task DistillBrandTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	runner := p.Deps.Runner
	if runner == nil {
		return done("skipped_unwired")
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	var st brandState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if st.Token != "" {
		return p.poll(ctx, task, st, in.lastAttempt())
	}
	active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID)
	if err != nil {
		return p.failed(ctx, task, err, in.lastAttempt())
	}
	if !active {
		runner.FailInactive(ctx, task.TenantID, task.RunID)
		return done("skipped_inactive")
	}

	mapper, err := p.mapper(ctx, st, in.Result)
	if err != nil {
		return StepOutput{}, err
	}
	err = runner.Prepare(ctx, task.TenantID, task.RunID, mapper)
	deferred, _ := mapper.(*deferredMap)
	switch {
	case deferred != nil && errors.Is(err, branddistill.ErrMapDeferred):
		return p.planMap(deferred.req)
	case errors.Is(err, branddistill.ErrWaiting):
		return snooze("sources ingesting", distillBrandPoll)
	case err != nil:
		return p.failed(ctx, task, err, in.lastAttempt())
	}

	if p.Deps.Runs == nil {
		return p.settle(ctx, task, runner.Finish(ctx, task.TenantID, task.RunID), in.lastAttempt())
	}
	token, err := p.Deps.Runs.StartFinishDetached(ctx, task.TenantID, task.RunID)
	switch {
	case errors.Is(err, branddistill.ErrRunGone):
		return done("skipped_run_gone")
	case err != nil:
		return retry("distill_brand", err)
	case token == "":
		// The model stages are already running, started before a restart.
		slot, err := p.Deps.Runs.FinishSlot(ctx, task.TenantID, task.RunID)
		if err != nil {
			return retry("distill_brand", err)
		}
		token = slot.Token
	}
	return brandWait(brandState{Token: token, Started: time.Now()})
}

// mapper is what Prepare maps the site with: river-service's answer to the
// map this job asked for, a deferral that asks for one, or, with nothing to
// defer to, the service's own mapper (nil when it has none).
func (p *DistillBrandProcessor) mapper(ctx context.Context, st brandState, r *StepResult) (branddistill.Mapper, error) {
	runs := p.Deps.Runs
	switch {
	case st.Map != nil:
		links, err := localOr(r, func(r *StepResult) ([]firecrawl.Link, error) {
			var links []firecrawl.Link
			err := firecrawl.Decode(firecrawl.Result{Status: r.Status, Body: r.Body, Err: r.Err}, func(c *firecrawl.Client) error {
				var err error
				links, err = c.Map(ctx, *st.Map)
				return err
			})
			return links, err
		})
		return mappedSite{links: links, err: err}, nil
	case runs != nil && runs.CanMap():
		return &deferredMap{}, nil
	}
	return nil, nil
}

// planMap asks river-service for the site map, or, inside ogen, makes it
// with the service's mapper.
func (p *DistillBrandProcessor) planMap(req firecrawl.MapRequest) (StepOutput, error) {
	call, err := firecrawl.Plan(func(c *firecrawl.Client) error { _, err := c.Map(context.Background(), req); return err })
	if err != nil {
		return StepOutput{}, err
	}
	raw, err := encodeState(brandState{Map: &req})
	if err != nil {
		return StepOutput{}, err
	}
	runs := p.Deps.Runs
	return StepOutput{State: raw, HTTP: &HTTPCall{
		Upstream: UpstreamFirecrawl, Method: call.Method, Path: call.Path, Body: call.Body,
		local: func(ctx context.Context) (any, error) { return runs.Map(ctx, req) },
	}}, nil
}

// poll checks the detached model stages the job waits on.
func (p *DistillBrandProcessor) poll(ctx context.Context, task DistillBrandTask, st brandState, lastAttempt bool) (StepOutput, error) {
	slot, err := p.Deps.Runs.FinishSlot(ctx, task.TenantID, task.RunID)
	switch {
	case errors.Is(err, branddistill.ErrRunGone):
		return done("skipped_run_gone")
	case err != nil:
		return retry("distill_brand", err)
	case slot.Token != st.Token:
		return retry("distill_brand", errors.New("another run took the brand distillation"))
	case slot.Done:
		return p.settle(ctx, task, slot.Err, lastAttempt)
	case slot.Running:
		return brandWait(st)
	default:
		// No heartbeat: ogen restarted under the run.
		return retry("distill_brand", errors.New("the brand distillation stopped without an outcome"))
	}
}

// settle ends the job on the model stages' outcome.
func (p *DistillBrandProcessor) settle(ctx context.Context, task DistillBrandTask, runErr error, lastAttempt bool) (StepOutput, error) {
	if runErr == nil {
		return done("distilled")
	}
	return p.failed(ctx, task, runErr, lastAttempt)
}

// failed retries the job, or on its last attempt fails the run so it never
// stays open.
func (p *DistillBrandProcessor) failed(ctx context.Context, task DistillBrandTask, err error, lastAttempt bool) (StepOutput, error) {
	if !lastAttempt {
		return retry("distill_brand", err)
	}
	slog.WarnContext(ctx, "brand distillation failed on its last attempt", logging.AttrComponent, "jobs.distill_brand",
		"run_id", task.RunID, logging.AttrError, err)
	p.Deps.Runner.Fail(ctx, task.TenantID, task.RunID, err)
	return done("failed")
}

func brandWait(st brandState) (StepOutput, error) {
	raw, err := encodeState(st)
	if err != nil {
		return StepOutput{}, err
	}
	return StepOutput{State: raw, Wait: analysisPollEvery}, nil
}

// deferredMap is a Mapper that defers the map to river-service, keeping the
// request Prepare asked for.
type deferredMap struct{ req firecrawl.MapRequest }

func (d *deferredMap) Map(_ context.Context, in firecrawl.MapRequest) ([]firecrawl.Link, error) {
	d.req = in
	return nil, branddistill.ErrMapDeferred
}

// mappedSite answers Prepare's map with river-service's result.
type mappedSite struct {
	links []firecrawl.Link
	err   error
}

func (m mappedSite) Map(context.Context, firecrawl.MapRequest) ([]firecrawl.Link, error) {
	return m.links, m.err
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
	return e.insert(ctx, DistillBrandTask{TenantID: tenantID, RunID: runID}, nil)
}
