package queues_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

type fakeAnalysisRunner struct {
	runErr   error
	editWait time.Duration
	// changedDuringRun reports the asset as edited after the run started.
	changedDuringRun bool
	ran              []assetanalysis.Lens
	reasons          []string
	failed           []assetanalysis.Lens
	rebriefs         []string
	backfills        []int
}

func (f *fakeAnalysisRunner) Run(_ context.Context, l assetanalysis.Lens, reason string) error {
	f.ran = append(f.ran, l)
	f.reasons = append(f.reasons, reason)
	return f.runErr
}

func (f *fakeAnalysisRunner) EditWait(_ context.Context, _ assetanalysis.Lens, since time.Time) (time.Duration, bool) {
	if since.IsZero() {
		return f.editWait, true
	}
	return f.editWait, f.changedDuringRun
}

func (f *fakeAnalysisRunner) Backfill(_ context.Context, perTenant int) (int, error) {
	f.backfills = append(f.backfills, perTenant)
	return 0, nil
}

func (f *fakeAnalysisRunner) Fail(_ context.Context, l assetanalysis.Lens, _ error) {
	f.failed = append(f.failed, l)
}

func (f *fakeAnalysisRunner) Rebrief(_ context.Context, _, campaignID string) error {
	f.rebriefs = append(f.rebriefs, campaignID)
	return nil
}

func TestAnalyzeAssetRetriesThenFails(t *testing.T) {
	runner := &fakeAnalysisRunner{runErr: errors.New("model down")}
	proc := &queues.AnalyzeAssetProcessor{Deps: queues.AnalysisDeps{Runner: runner}}
	campaignID := "camp-1"
	task := queues.AnalyzeAssetTask{TenantID: "t1", AssetID: "a1", CampaignID: &campaignID, Reason: "attached"}

	if err := proc.Process(t.Context(), task, false); err == nil {
		t.Fatal("an early failed attempt must return its error so River retries")
	}
	if len(runner.failed) != 0 {
		t.Fatal("an early attempt marked the lens failed")
	}
	if err := proc.Process(t.Context(), task, true); err != nil {
		t.Fatalf("the last attempt should settle the lens, got %v", err)
	}
	if len(runner.failed) != 1 || runner.failed[0].AssetID != "a1" || *runner.failed[0].CampaignID != "camp-1" {
		t.Fatalf("failed = %+v", runner.failed)
	}
}

func TestAnalysisJobsAreNoOpsWithoutARunner(t *testing.T) {
	proc := &queues.AnalyzeAssetProcessor{}
	if err := proc.Process(t.Context(), queues.AnalyzeAssetTask{TenantID: "t1", AssetID: "a1"}, true); err != nil {
		t.Fatal(err)
	}
	rebrief := &queues.ReanalyzeCampaignProcessor{}
	if err := rebrief.Process(t.Context(), queues.ReanalyzeCampaignTask{TenantID: "t1", CampaignID: "c1"}); err != nil {
		t.Fatal(err)
	}
}

func TestReanalyzeCampaignDelegates(t *testing.T) {
	runner := &fakeAnalysisRunner{}
	proc := &queues.ReanalyzeCampaignProcessor{Deps: queues.AnalysisDeps{Runner: runner}}
	if err := proc.Process(t.Context(), queues.ReanalyzeCampaignTask{TenantID: "t1", CampaignID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.rebriefs) != 1 || runner.rebriefs[0] != "c1" {
		t.Fatalf("rebriefs = %v", runner.rebriefs)
	}
}

func TestEditedAnalysisSnoozesUntilTheAssetIsQuiet(t *testing.T) {
	runner := &fakeAnalysisRunner{editWait: 90 * time.Second}
	proc := &queues.AnalyzeAssetProcessor{Deps: queues.AnalysisDeps{Runner: runner}}
	task := queues.AnalyzeAssetTask{TenantID: "t1", AssetID: "a1", Reason: assetanalysis.ReasonEdited}

	err := proc.Process(t.Context(), task, false)
	snooze, ok := errors.AsType[*queues.SnoozeError](err)
	if !ok || snooze.After != 90*time.Second {
		t.Fatalf("a recently edited asset should snooze for the rest of the debounce, got %v", err)
	}
	if len(runner.ran) != 0 {
		t.Fatal("a snoozed job ran the analysis")
	}

	runner.editWait = 0
	if err := proc.Process(t.Context(), task, false); err != nil {
		t.Fatal(err)
	}
	if len(runner.ran) != 1 || runner.reasons[0] != assetanalysis.ReasonEdited {
		t.Fatalf("ran = %+v reasons = %v", runner.ran, runner.reasons)
	}
}

func TestOnlyEditedAnalysesWait(t *testing.T) {
	runner := &fakeAnalysisRunner{editWait: time.Minute}
	proc := &queues.AnalyzeAssetProcessor{Deps: queues.AnalysisDeps{Runner: runner}}
	for _, reason := range []string{assetanalysis.ReasonIngested, assetanalysis.ReasonAttached, assetanalysis.ReasonRetry, assetanalysis.ReasonBackfill} {
		if err := proc.Process(t.Context(), queues.AnalyzeAssetTask{TenantID: "t1", AssetID: "a1", Reason: reason}, false); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
	}
	if len(runner.ran) != 4 {
		t.Fatalf("ran %d of 4 immediate analyses", len(runner.ran))
	}
}

func TestBackfillQueuesABatchPerTenant(t *testing.T) {
	runner := &fakeAnalysisRunner{}
	if err := (&queues.BackfillAssetAnalysesProcessor{Deps: queues.AnalysisDeps{Runner: runner}}).Process(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := (&queues.BackfillAssetAnalysesProcessor{Deps: queues.AnalysisDeps{Runner: runner, BackfillBatch: 5}}).Process(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(runner.backfills) != 2 || runner.backfills[0] != queues.DefaultBackfillBatch || runner.backfills[1] != 5 {
		t.Fatalf("backfill batches = %v", runner.backfills)
	}
	if err := (&queues.BackfillAssetAnalysesProcessor{}).Process(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAnalysisRerunsWhenTheAssetChangedMidRun(t *testing.T) {
	runner := &fakeAnalysisRunner{changedDuringRun: true, editWait: 2 * time.Minute}
	proc := &queues.AnalyzeAssetProcessor{Deps: queues.AnalysisDeps{Runner: runner}}

	err := proc.Process(t.Context(), queues.AnalyzeAssetTask{TenantID: "t1", AssetID: "a1", Reason: assetanalysis.ReasonRetry}, false)
	snooze, ok := errors.AsType[*queues.SnoozeError](err)
	if !ok || snooze.After != 2*time.Minute || len(runner.ran) != 1 {
		t.Fatalf("a run raced by an edit should snooze to re-read it, got %v after %d runs", err, len(runner.ran))
	}
}
