package queues_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

type fakeAnalysisRunner struct {
	runErr   error
	ran      []assetanalysis.Lens
	failed   []assetanalysis.Lens
	rebriefs []string
}

func (f *fakeAnalysisRunner) Run(_ context.Context, l assetanalysis.Lens) error {
	f.ran = append(f.ran, l)
	return f.runErr
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
