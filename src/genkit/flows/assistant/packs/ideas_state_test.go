package packs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

type stateIdeas struct {
	repository.IdeaRepository
	scoped string
	err    error
}

func (r *stateIdeas) Query(_ context.Context, q models.IdeaQuery) (*models.IdeaQueryResult, error) {
	r.scoped = q.ScopeCampaign
	if r.err != nil {
		return nil, r.err
	}
	return &models.IdeaQueryResult{
		TotalUnfiltered: 40,
		Piles:           map[string]int{"waiting": 12, "yes": 20, "later": 3, "no": 5},
		Facets: map[string][]models.AssetFacetCount{
			models.IdeaFacetOrigin:      {{Value: "asset_analysis", Count: 9}, {Value: "content_plan"}, {Value: "manual", Count: 3}},
			models.IdeaFacetSourceAsset: {{Value: "a1", Count: 5}, {Value: models.AssetFilterNone, Count: 3}},
			models.IdeaFacetCampaign:    {{Value: "c1", Count: 4}, {Value: "c9", Count: 1}, {Value: models.AssetFilterNone, Count: 7}},
		},
		SourceAssetLabels: []models.IdeaFacetAsset{{ID: "a1", Title: "Brochure"}},
	}, nil
}

func (r *stateIdeas) PlannedIdeas(context.Context, string, int) ([]models.Idea, int, error) {
	day, _ := models.ParseCalendarDate("2026-12-02")
	return []models.Idea{
		{ID: "i1", Title: "Launch teaser", Plan: models.IdeaPlan{TargetDate: &day, PlatformID: new("linkedin")}},
		{ID: "i2", Title: "Behind the scenes"},
	}, 7, nil
}

func TestIdeasBegin(t *testing.T) {
	repo := &stateIdeas{}
	begin := ideasBegin(Deps{Ideas: ideas.New(repo, nil), Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring reopening"}}})

	_, block, err := begin(t.Context(), stateTurn(workspace, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<ideas>\nIdeas in this workspace: 40 — 12 waiting for a decision, 20 yes, 3 later, 5 no.",
		"Waiting, by origin: 9 from asset analyses, 3 written by hand.",
		`Waiting, top sources: 5 from "Brochure" (a1).`,
		`Waiting, top campaigns: 7 in the workspace backlog, 4 in "Spring reopening" (c1), 1 in campaign c9.`,
		`Planned, no post made yet: 7 — "Launch teaser" (i1) for 2026-12-02 on linkedin; "Behind the scenes" (i2); and 5 more.`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block lacks %q:\n%s", want, block)
		}
	}

	_, block, _ = begin(t.Context(), stateTurn(campaign1, "Spring reopening"))
	if !strings.Contains(block, `Ideas in campaign "Spring reopening": 40`) || repo.scoped != "c1" {
		t.Fatalf("campaign block (scoped %q):\n%s", repo.scoped, block)
	}

	// A state that can't be read leaves the block out, not the pack.
	repo.err = errors.New("db down")
	if _, block, err := begin(t.Context(), stateTurn(workspace, "")); err != nil || block != "" {
		t.Fatalf("failed read = %q, %v", block, err)
	}
}

func TestRenderIdeasState_Empty(t *testing.T) {
	block := renderIdeasState(&ideas.State{Piles: map[string]int{}}, "", nil)
	want := "<ideas>\nIdeas in this workspace: 0.\nNo planned idea is waiting to be made into a post.\n</ideas>"
	if block != want {
		t.Fatalf("block =\n%s\nwant\n%s", block, want)
	}
}
