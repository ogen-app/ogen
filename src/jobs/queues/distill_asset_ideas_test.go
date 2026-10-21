package queues_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
)

// River rejects unique opts it cannot enforce, so the distill job must insert
// for real; a run queued twice still runs once.
func TestDistillJobInsertsOncePerRun(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.MigrateRiver(t.Context(), db.DB); err != nil {
		t.Fatal(err)
	}
	enq, err := queues.NewInsertOnlyEnqueuer(db)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := enq.EnqueueDistillIdeas(t.Context(), "t1", "run-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := enq.EnqueueDistillIdeas(t.Context(), "t1", "run-2"); err != nil {
		t.Fatal(err)
	}
	n, err := db.NewSelect().Table("river_job").Where("kind = ?", queues.DistillAssetIdeasKind).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("distill jobs = %d, want one per run", n)
	}
}

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
