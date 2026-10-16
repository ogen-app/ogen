package queues_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeDistillRunner struct {
	err    error
	runs   []string
	failed []string
}

func (f *fakeDistillRunner) Distill(_ context.Context, _, runID string) error {
	f.runs = append(f.runs, runID)
	return f.err
}

func (f *fakeDistillRunner) FailDistill(_ context.Context, _, runID string, _ error) {
	f.failed = append(f.failed, runID)
}

func TestDistillJobFailsTheRunOnItsLastAttempt(t *testing.T) {
	runner := &fakeDistillRunner{err: errors.New("model down")}
	p := &queues.DistillAssetIdeasProcessor{Deps: queues.AnalysisDeps{Distiller: runner}}
	task := queues.DistillAssetIdeasTask{TenantID: "t1", RunID: "run-1"}

	if err := p.Process(t.Context(), task, false); err == nil || len(runner.failed) != 0 {
		t.Fatalf("early attempt: err = %v, failed %v; want a retry", err, runner.failed)
	}
	if err := p.Process(t.Context(), task, true); err != nil || len(runner.failed) != 1 {
		t.Fatalf("last attempt: err = %v, failed %v; want the run failed", err, runner.failed)
	}
	idle := &queues.DistillAssetIdeasProcessor{}
	if err := idle.Process(t.Context(), task, true); err != nil {
		t.Fatalf("no distiller: %v", err)
	}
}
