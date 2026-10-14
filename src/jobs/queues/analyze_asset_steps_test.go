package queues_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// fakeRuns scripts a lens's detached run: StartDetached hands out token
// (or "" when busy), and Slot reads through slots one call at a time.
type fakeRuns struct {
	token   string
	slots   []assetanalysis.RunSlot
	started int
	touched int
}

func (f *fakeRuns) StartDetached(context.Context, assetanalysis.Lens, string) (string, error) {
	f.started++
	return f.token, nil
}

func (f *fakeRuns) Slot(context.Context, assetanalysis.Lens) (assetanalysis.RunSlot, error) {
	s := f.slots[0]
	if len(f.slots) > 1 {
		f.slots = f.slots[1:]
	}
	return s, nil
}

func (f *fakeRuns) Touch(context.Context, assetanalysis.Lens) { f.touched++ }

// driveAnalysis steps an analyze_asset job the way river-service does,
// following waits, and returns its last output and how many waits it took.
func driveAnalysis(t *testing.T, d queues.AnalysisDeps, attempt int) (queues.StepOutput, int) {
	t.Helper()
	steps := queues.NewJobSteps(queues.Deps{Analysis: d})
	in := queues.StepInput{Kind: queues.AnalyzeAssetKind, Args: []byte(`{"tenant_id":"t1","asset_id":"a1","reason":"ingested"}`), Attempt: attempt, MaxAttempts: 5}
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

func TestAnalysisWaitsOnItsDetachedRun(t *testing.T) {
	runner := &fakeAnalysisRunner{}
	runs := &fakeRuns{token: "r1", slots: []assetanalysis.RunSlot{
		{Token: "r1", Running: true}, {Token: "r1", Done: true},
	}}
	out, waits := driveAnalysis(t, queues.AnalysisDeps{Runner: runner, Runs: runs}, 1)
	if out.Done == nil || out.Done.Outcome != "analyzed" || waits != 2 || runs.started != 1 || runs.touched < 3 {
		t.Fatalf("out %+v waits %d started %d touched %d", out.Done, waits, runs.started, runs.touched)
	}
	if len(runner.ran) != 0 {
		t.Error("the step ran the analysis inline")
	}
}

func TestAnalysisAdoptsTheRunInFlight(t *testing.T) {
	// The job restarted (a deploy) while its run was in flight: the claim
	// fails and the job waits on the run already going.
	runs := &fakeRuns{slots: []assetanalysis.RunSlot{
		{Token: "r0", Running: true}, {Token: "r0", Done: true},
	}}
	out, waits := driveAnalysis(t, queues.AnalysisDeps{Runner: &fakeAnalysisRunner{}, Runs: runs}, 1)
	if out.Done == nil || waits != 1 || runs.started != 1 {
		t.Fatalf("out %+v waits %d", out, waits)
	}
}

func TestAnalysisRunOutcomes(t *testing.T) {
	failed := assetanalysis.RunSlot{Token: "r1", Done: true, Err: errors.New("model down")}
	lost := assetanalysis.RunSlot{Token: "r1"}

	out, _ := driveAnalysis(t, queues.AnalysisDeps{Runner: &fakeAnalysisRunner{}, Runs: &fakeRuns{token: "r1", slots: []assetanalysis.RunSlot{failed}}}, 1)
	if out.Retry == nil || out.Retry.After != 0 {
		t.Errorf("failed run: %+v, want a retry", out)
	}
	runner := &fakeAnalysisRunner{}
	out, _ = driveAnalysis(t, queues.AnalysisDeps{Runner: runner, Runs: &fakeRuns{token: "r1", slots: []assetanalysis.RunSlot{failed}}}, 5)
	if out.Done == nil || len(runner.failed) != 1 {
		t.Errorf("failed run on the last attempt: %+v, failed %d; want the lens failed", out, len(runner.failed))
	}
	out, _ = driveAnalysis(t, queues.AnalysisDeps{Runner: &fakeAnalysisRunner{}, Runs: &fakeRuns{token: "r1", slots: []assetanalysis.RunSlot{lost}}}, 1)
	if out.Retry == nil {
		t.Errorf("lost heartbeat: %+v, want a retry", out)
	}
	runner = &fakeAnalysisRunner{changedDuringRun: true}
	out, _ = driveAnalysis(t, queues.AnalysisDeps{Runner: runner, Runs: &fakeRuns{token: "r1", slots: []assetanalysis.RunSlot{{Token: "r1", Done: true}}}}, 1)
	if out.Retry == nil || out.Retry.After <= 0 {
		t.Errorf("asset changed mid-run: %+v, want a snooze", out)
	}
}
