package queues_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

type fakeBrandRunner struct {
	err      error
	runs     []string
	failed   []string
	inactive []string
}

func (f *fakeBrandRunner) Prepare(_ context.Context, _, runID string, _ branddistill.Mapper) error {
	f.runs = append(f.runs, runID)
	return f.err
}

func (f *fakeBrandRunner) Finish(context.Context, string, string) error { return nil }

func (f *fakeBrandRunner) Fail(_ context.Context, _, runID string, _ error) {
	f.failed = append(f.failed, runID)
}

func (f *fakeBrandRunner) FailInactive(_ context.Context, _, runID string) {
	f.inactive = append(f.inactive, runID)
}

func (f *fakeBrandRunner) Expire(context.Context) (int64, error) { return 0, nil }

type tenantStatus struct {
	status string
	err    error
}

func (t tenantStatus) GetStatus(context.Context, string) (string, error) { return t.status, t.err }

func TestDistillBrandJob(t *testing.T) {
	task := queues.DistillBrandTask{TenantID: "t1", RunID: "run-1"}
	active := tenantStatus{status: models.TenantStatusActive}

	t.Run("sources still ingesting snooze the job", func(t *testing.T) {
		runner := &fakeBrandRunner{err: branddistill.ErrWaiting}
		p := &queues.DistillBrandProcessor{Deps: queues.BrandDistillDeps{Runner: runner, Tenants: active}}
		err := p.Process(t.Context(), task, true)
		if err == nil || errors.Is(err, branddistill.ErrWaiting) || len(runner.failed) != 0 {
			t.Fatalf("err = %v, failed = %v; want a snooze, not a failure", err, runner.failed)
		}
	})

	t.Run("the last attempt fails the run", func(t *testing.T) {
		runner := &fakeBrandRunner{err: errors.New("model down")}
		p := &queues.DistillBrandProcessor{Deps: queues.BrandDistillDeps{Runner: runner, Tenants: active}}
		if err := p.Process(t.Context(), task, false); err == nil || len(runner.failed) != 0 {
			t.Fatalf("an early attempt: err = %v, failed = %v", err, runner.failed)
		}
		if err := p.Process(t.Context(), task, true); err != nil || len(runner.failed) != 1 {
			t.Fatalf("the last attempt: err = %v, failed = %v", err, runner.failed)
		}
	})

	t.Run("a tenant lookup that keeps failing still closes the run", func(t *testing.T) {
		runner := &fakeBrandRunner{}
		p := &queues.DistillBrandProcessor{Deps: queues.BrandDistillDeps{Runner: runner, Tenants: tenantStatus{err: errors.New("db down")}}}
		if err := p.Process(t.Context(), task, false); err == nil || len(runner.failed) != 0 {
			t.Fatalf("an early attempt: err = %v, failed = %v", err, runner.failed)
		}
		if err := p.Process(t.Context(), task, true); err != nil || len(runner.failed) != 1 || len(runner.runs) != 0 {
			t.Fatalf("the last attempt: err = %v, failed = %v, runs = %v", err, runner.failed, runner.runs)
		}
	})

	t.Run("an inactive workspace's run is failed", func(t *testing.T) {
		runner := &fakeBrandRunner{}
		p := &queues.DistillBrandProcessor{Deps: queues.BrandDistillDeps{Runner: runner, Tenants: tenantStatus{status: models.TenantStatusSuspended}}}
		if err := p.Process(t.Context(), task, false); err != nil || len(runner.inactive) != 1 || len(runner.runs) != 0 {
			t.Fatalf("err = %v, inactive = %v, runs = %v", err, runner.inactive, runner.runs)
		}
	})
}
