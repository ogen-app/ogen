package packs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

type stateAssets struct {
	repository.AssetRepository
	scoped string
	err    error
}

func (r *stateAssets) Query(_ context.Context, q models.AssetQuery) (*models.AssetQueryResult, error) {
	r.scoped = q.ScopeCampaign
	if r.err != nil {
		return nil, r.err
	}
	return &models.AssetQueryResult{TotalUnfiltered: 64, Facets: map[string][]models.AssetFacetCount{
		models.AssetFacetStatus: {{Value: "ready", Count: 61}, {Value: "pending", Count: 2}, {Value: "failed", Count: 1}},
	}}, nil
}

type stateAnalyses struct {
	repository.AssetAnalysisRepository
	lens *string
}

func (r *stateAnalyses) State(_ context.Context, campaignID *string, _ time.Time, names int) (*repository.AnalysisState, error) {
	r.lens = campaignID
	return &repository.AnalysisState{
		Lenses: map[models.AssetAnalysisStatus]int{
			models.AssetAnalysisStatusReady: 49, models.AssetAnalysisStatusPending: 12, models.AssetAnalysisStatusFailed: 3,
		},
		Waiting:   []repository.AssetTitle{{ID: "a1", Title: "Founder interview"}, {ID: "a2", Title: "Wedding packages"}}[:min(names, 2)],
		Failed:    []repository.AssetTitle{{ID: "a3", Title: "Coast & Table review"}},
		Undecided: []repository.LensIdeas{{Ideas: 29}, {CampaignID: new("c1"), CampaignName: "Spring reopening", Ideas: 12}},
	}, nil
}

func stateTurn(scope viewcontext.Scope, title string) assistant.Turn {
	r := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "assets", Scope: scope}}
	if title != "" {
		r.Scope = &viewresolve.Item{Kind: viewcontext.KindCampaign, ID: scope.ID, Title: title}
	}
	return assistant.Turn{View: r}
}

func TestAssetsBegin(t *testing.T) {
	as, an := &stateAssets{}, &stateAnalyses{}
	begin := assetsBegin(Deps{Assets: as, Analyses: an})

	_, block, err := begin(t.Context(), stateTurn(workspace, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<assets>\nThis workspace's bank: 64 assets (2 still being read in, 1 couldn't be read).",
		`Analyses for the workspace lens: 49 done, 12 waiting in the queue: "Founder interview" (a1), "Wedding packages" (a2) and 10 more, 3 failed: "Coast & Table review" (a3) and 2 more.`,
		`Undecided ideas from the analyses: 41 (29 from the workspace lens, 12 from campaign "Spring reopening").`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block lacks %q:\n%s", want, block)
		}
	}
	if as.scoped != "" || an.lens != nil {
		t.Fatalf("workspace read scoped to %q / %v", as.scoped, an.lens)
	}

	_, block, _ = begin(t.Context(), stateTurn(campaign1, "Spring reopening"))
	if !strings.Contains(block, `The sources of campaign "Spring reopening": 64 assets`) || !strings.Contains(block, "this campaign's lens") {
		t.Fatalf("campaign block:\n%s", block)
	}
	if as.scoped != "c1" || an.lens == nil || *an.lens != "c1" {
		t.Fatalf("campaign read scoped to %q / %v", as.scoped, an.lens)
	}

	// A state that can't be read leaves the block out, not the pack.
	as.err = errors.New("db down")
	if _, block, err := begin(t.Context(), stateTurn(workspace, "")); err != nil || block != "" {
		t.Fatalf("failed read = %q, %v", block, err)
	}
	if _, block, err := assetsBegin(Deps{Assets: as})(t.Context(), stateTurn(workspace, "")); err != nil || block != "" {
		t.Fatalf("no analyses = %q, %v", block, err)
	}
}

func TestRenderAssetsState_Empty(t *testing.T) {
	_, block, err := assetsBegin(Deps{Assets: &emptyAssets{}, Analyses: &emptyAnalyses{}})(t.Context(), stateTurn(workspace, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := "<assets>\nThis workspace's bank: 0 assets.\nAnalyses for the workspace lens: 0 done.\nNo ideas from the analyses are waiting for a decision.\n</assets>"
	if block != want {
		t.Fatalf("block =\n%s\nwant\n%s", block, want)
	}
}

type emptyAssets struct{ repository.AssetRepository }

func (emptyAssets) Query(context.Context, models.AssetQuery) (*models.AssetQueryResult, error) {
	return &models.AssetQueryResult{}, nil
}

type emptyAnalyses struct {
	repository.AssetAnalysisRepository
}

func (emptyAnalyses) State(context.Context, *string, time.Time, int) (*repository.AnalysisState, error) {
	return &repository.AnalysisState{Lenses: map[models.AssetAnalysisStatus]int{}}, nil
}

// readAssetsState reads the thread's scope and gives the state to the card.
func TestReadAssetsState(t *testing.T) {
	as, an := &stateAssets{}, &stateAnalyses{}
	d := Deps{Assets: as, Analyses: an}

	ctx, _ := turn(t, campaign1, nil)
	ctx = withAssetsTurn(ctx)
	out, err := readAssetsState(ctx, d)
	if err != nil || out.State.Total != 64 || out.State.AnalysisFailed != 3 || len(out.State.FailedAssets) != 1 {
		t.Fatalf("state = %+v, %v", out, err)
	}
	if as.scoped != "c1" || an.lens == nil || *an.lens != "c1" {
		t.Fatalf("read scoped to %q / %v; want the thread's campaign", as.scoped, an.lens)
	}

	ctx, _ = turn(t, workspace, nil)
	if _, err := readAssetsState(ctx, d); err != nil || as.scoped != "" || an.lens != nil {
		t.Fatalf("workspace read scoped to %q / %v, %v", as.scoped, an.lens, err)
	}
	as.err = errors.New("db down")
	if _, err := readAssetsState(ctx, d); err == nil {
		t.Fatal("a failed read is an error")
	}
}
