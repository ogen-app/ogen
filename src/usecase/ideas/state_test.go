package ideas

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// stateRepo answers the state's two reads and keeps the query it was asked.
type stateRepo struct {
	repository.IdeaRepository
	q       models.IdeaQuery
	planned string
}

func (r *stateRepo) Query(_ context.Context, q models.IdeaQuery) (*models.IdeaQueryResult, error) {
	r.q = q
	if q.ScopeCampaign == "gone" {
		return nil, sql.ErrNoRows
	}
	none := models.AssetFilterNone
	return &models.IdeaQueryResult{
		TotalUnfiltered: 40,
		Piles:           map[string]int{"waiting": 12, "yes": 20, "later": 3, "no": 5},
		Facets: map[string][]models.AssetFacetCount{
			models.IdeaFacetOrigin: {{Value: "asset_analysis", Count: 9}, {Value: "content_plan"}, {Value: "assistant", Count: 1}, {Value: "manual", Count: 2}},
			models.IdeaFacetSourceAsset: {{Value: "a1", Count: 5}, {Value: "a2", Count: 3}, {Value: "a3", Count: 1},
				{Value: "a4", Count: 1}, {Value: none, Count: 2}},
			models.IdeaFacetCampaign: {{Value: "c1", Count: 4}, {Value: "c2", Count: 1}, {Value: "c3"}, {Value: none, Count: 7}},
		},
		SourceAssetLabels: []models.IdeaFacetAsset{{ID: "a1", Title: "Brochure"}, {ID: "a2", Title: "Press kit"}},
	}, nil
}

func (r *stateRepo) PlannedIdeas(_ context.Context, campaignID string, limit int) ([]models.Idea, int, error) {
	r.planned = campaignID
	return []models.Idea{{ID: "i1", Title: "Launch teaser"}}[:min(1, limit)], 6, nil
}

// The state counts the piles, breaks the waiting ideas down by origin,
// source and (outside a campaign) campaign, the backlog ranked by its
// count, and names the soonest planned ideas.
func TestReadState(t *testing.T) {
	repo := &stateRepo{}
	s := New(repo, nil)
	st, err := s.ReadState(t.Context(), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(repo.q.Pile.Any, []string{models.IdeaPileWaiting}) || !slices.Contains(repo.q.Facets, models.IdeaFacetCampaign) {
		t.Fatalf("query = %+v", repo.q)
	}
	if st.Total != 40 || st.Piles["yes"] != 20 || len(st.WaitingByOrigin) != 3 || st.WaitingByOrigin[0].Value != "asset_analysis" {
		t.Fatalf("state = %+v", st)
	}
	if len(st.WaitingBySource) != 3 || st.WaitingBySource[0] != (SourceCount{AssetID: "a1", Title: "Brochure", Count: 5}) {
		t.Fatalf("by source = %+v", st.WaitingBySource)
	}
	var camps []string
	for _, c := range st.WaitingByCampaign {
		camps = append(camps, c.Value)
	}
	if !slices.Equal(camps, []string{models.AssetFilterNone, "c1", "c2"}) {
		t.Fatalf("by campaign = %v", camps)
	}
	if st.PlannedTotal != 6 || len(st.Planned) != 1 || repo.planned != "" {
		t.Fatalf("planned = %+v (%d)", st.Planned, st.PlannedTotal)
	}

	if st, _ := s.ReadState(t.Context(), "c1", time.Now()); slices.Contains(repo.q.Facets, models.IdeaFacetCampaign) || repo.planned != "c1" || st.CampaignID != "c1" {
		t.Fatalf("in a campaign: query %+v, planned scope %q", repo.q, repo.planned)
	}
	if _, err := s.ReadState(t.Context(), "gone", time.Now()); !errors.Is(err, ErrScopeNotFound) {
		t.Fatalf("gone campaign = %v", err)
	}
}
