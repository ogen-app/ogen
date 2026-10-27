package queues_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

// fakeDistillRuns scripts a distill run's detached call, like fakeRuns.
type fakeDistillRuns struct {
	token   string
	gone    bool
	slots   []runslot.Slot
	started int
}

func (f *fakeDistillRuns) StartDistillDetached(context.Context, string, string) (string, error) {
	f.started++
	if f.gone {
		return "", assetanalysis.ErrDistillGone
	}
	return f.token, nil
}

func (f *fakeDistillRuns) DistillSlot(context.Context, string, string) (runslot.Slot, error) {
	s := f.slots[0]
	if len(f.slots) > 1 {
		f.slots = f.slots[1:]
	}
	return s, nil
}

func driveDistill(t *testing.T, d queues.AnalysisDeps, attempt int) (queues.StepOutput, int) {
	t.Helper()
	steps := queues.NewJobSteps(queues.Deps{Analysis: d})
	in := queues.StepInput{Kind: queues.DistillAssetIdeasKind, Args: []byte(`{"tenant_id":"t1","run_id":"d1"}`), Attempt: attempt, MaxAttempts: 5}
	waits := 0
	for range 20 {
		out, err := steps.Step(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if out.Wait <= 0 {
			return out, waits
		}
		waits++
		in.State = out.State
	}
	t.Fatal("job did not finish")
	return queues.StepOutput{}, 0
}

func TestDistillWaitsOnItsDetachedRun(t *testing.T) {
	runner := &fakeDistillRunner{}
	runs := &fakeDistillRuns{token: "r1", slots: []runslot.Slot{{Token: "r1", Running: true}, {Token: "r1", Done: true}}}
	out, waits := driveDistill(t, queues.AnalysisDeps{Distiller: runner, DistillRuns: runs}, 1)
	if out.Done == nil || out.Done.Outcome != "distilled" || waits != 2 || runs.started != 1 {
		t.Fatalf("out %+v waits %d started %d", out.Done, waits, runs.started)
	}
	if len(runner.runs) != 0 {
		t.Error("the step ran the distill inline")
	}
}

func TestDistillAdoptsTheRunInFlight(t *testing.T) {
	runs := &fakeDistillRuns{slots: []runslot.Slot{{Token: "r0", Running: true}, {Token: "r0", Done: true}}}
	out, waits := driveDistill(t, queues.AnalysisDeps{Distiller: &fakeDistillRunner{}, DistillRuns: runs}, 1)
	if out.Done == nil || waits != 1 {
		t.Fatalf("out %+v waits %d", out, waits)
	}
}

func TestDistillRunOutcomes(t *testing.T) {
	failed := runslot.Slot{Token: "r1", Done: true, Err: errors.New("model down")}

	out, _ := driveDistill(t, queues.AnalysisDeps{Distiller: &fakeDistillRunner{}, DistillRuns: &fakeDistillRuns{token: "r1", slots: []runslot.Slot{failed}}}, 1)
	if out.Retry == nil {
		t.Errorf("failed run: %+v, want a retry", out)
	}
	runner := &fakeDistillRunner{}
	out, _ = driveDistill(t, queues.AnalysisDeps{Distiller: runner, DistillRuns: &fakeDistillRuns{token: "r1", slots: []runslot.Slot{failed}}}, 5)
	if out.Done == nil || len(runner.failed) != 1 {
		t.Errorf("failed run on the last attempt: %+v, failed %d; want the run failed", out, len(runner.failed))
	}
	out, _ = driveDistill(t, queues.AnalysisDeps{Distiller: &fakeDistillRunner{}, DistillRuns: &fakeDistillRuns{token: "r1", slots: []runslot.Slot{{Token: "r1"}}}}, 1)
	if out.Retry == nil {
		t.Errorf("lost heartbeat: %+v, want a retry", out)
	}
	out, _ = driveDistill(t, queues.AnalysisDeps{Distiller: &fakeDistillRunner{}, DistillRuns: &fakeDistillRuns{gone: true}}, 1)
	if out.Done == nil || out.Done.Outcome != "skipped_run_gone" {
		t.Errorf("run gone: %+v", out)
	}
}
