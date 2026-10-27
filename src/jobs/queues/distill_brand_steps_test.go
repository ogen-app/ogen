package queues_test

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

// mappingBrandRunner maps the site once, as Prepare does for a run with a
// website, and records the links it got.
type mappingBrandRunner struct {
	fakeBrandRunner
	mapped   bool
	links    []firecrawl.Link
	finished int
}

func (f *mappingBrandRunner) Prepare(ctx context.Context, _, runID string, mapper branddistill.Mapper) error {
	f.runs = append(f.runs, runID)
	if f.mapped {
		return nil
	}
	links, err := mapper.Map(ctx, firecrawl.MapRequest{URL: "https://acme.test", Limit: 200})
	if err != nil {
		return err
	}
	f.mapped, f.links = true, links
	return nil
}

func (f *mappingBrandRunner) Finish(context.Context, string, string) error {
	f.finished++
	return nil
}

type fakeBrandRuns struct {
	token string
	slots []runslot.Slot
}

func (f *fakeBrandRuns) StartFinishDetached(context.Context, string, string) (string, error) {
	return f.token, nil
}

func (f *fakeBrandRuns) FinishSlot(context.Context, string, string) (runslot.Slot, error) {
	s := f.slots[0]
	if len(f.slots) > 1 {
		f.slots = f.slots[1:]
	}
	return s, nil
}

func (f *fakeBrandRuns) Map(context.Context, firecrawl.MapRequest) ([]firecrawl.Link, error) {
	return nil, nil
}

func (f *fakeBrandRuns) CanMap() bool { return true }

func TestDistillBrandMapsThroughRiverServiceAndWaitsOnTheModel(t *testing.T) {
	runner := &mappingBrandRunner{}
	runs := &fakeBrandRuns{token: "r1", slots: []runslot.Slot{{Token: "r1", Running: true}, {Token: "r1", Done: true}}}
	steps := queues.NewJobSteps(queues.Deps{BrandDistill: queues.BrandDistillDeps{
		Runner: runner, Runs: runs, Tenants: tenantStatus{status: models.TenantStatusActive},
	}})
	in := queues.StepInput{Kind: queues.DistillBrandKind, Args: []byte(`{"tenant_id":"t1","run_id":"b1"}`), Attempt: 1, MaxAttempts: 5}

	out, err := steps.Step(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.HTTP == nil || out.HTTP.Upstream != queues.UpstreamFirecrawl || out.HTTP.Path != "/v2/map" {
		t.Fatalf("first step = %+v, want the site map through river-service", out)
	}

	in.State, in.Result = out.State, &queues.StepResult{Status: 200, Body: []byte(`{"success":true,"links":["https://acme.test/","https://acme.test/about"]}`)}
	waits := 0
	for range 10 {
		out, err = steps.Step(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if out.Wait <= 0 {
			break
		}
		waits++
		in.State, in.Result = out.State, nil
	}
	if out.Done == nil || out.Done.Outcome != "distilled" || waits != 2 {
		t.Fatalf("out = %+v after %d waits, want distilled after waiting on the model", out, waits)
	}
	if len(runner.links) != 2 || runner.finished != 0 {
		t.Fatalf("links = %v, finished inline %d; want the map's links and no inline model run", runner.links, runner.finished)
	}
}

func TestDistillBrandSnoozesWhileSourcesIngest(t *testing.T) {
	runner := &fakeBrandRunner{err: branddistill.ErrWaiting}
	steps := queues.NewJobSteps(queues.Deps{BrandDistill: queues.BrandDistillDeps{
		Runner: runner, Runs: &fakeBrandRuns{}, Tenants: tenantStatus{status: models.TenantStatusActive},
	}})
	out, err := steps.Step(t.Context(), queues.StepInput{Kind: queues.DistillBrandKind, Args: []byte(`{"tenant_id":"t1","run_id":"b1"}`), Attempt: 5, MaxAttempts: 5})
	if err != nil || out.Retry == nil || out.Retry.After <= 0 || len(runner.failed) != 0 {
		t.Fatalf("out = %+v, err = %v, failed = %v; want a snooze even on the last attempt", out, err, runner.failed)
	}
}
