package ideas

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// How much of each breakdown the state names: the largest few, and the
// soonest planned ideas.
const (
	StateTop     = 3
	StatePlanned = 5
)

// State is one scope's ideas at a glance: how they sit across the piles,
// where the waiting ones come from, and the planned ideas no post was made
// from yet.
type State struct {
	CampaignID string
	// Total counts every idea in scope; Piles each pile's.
	Total int
	Piles map[string]int
	// WaitingByOrigin counts the waiting ideas by origin, the empty origins
	// left out.
	WaitingByOrigin []models.AssetFacetCount
	// WaitingBySource names the source assets most waiting ideas came from,
	// and WaitingByCampaign, outside a campaign, the campaigns most are in
	// (models.AssetFilterNone for the workspace backlog); StateTop of each.
	WaitingBySource   []SourceCount
	WaitingByCampaign []models.AssetFacetCount
	// Planned is the soonest StatePlanned of PlannedTotal planned ideas.
	Planned      []models.Idea
	PlannedTotal int
}

// SourceCount is how many waiting ideas one source asset gave.
type SourceCount struct {
	AssetID string
	Title   string
	Count   int
}

// ReadState reads the state of campaignID's ideas, or with campaignID empty
// every idea's. It returns ErrScopeNotFound for a campaign that is gone.
func (s *Service) ReadState(ctx context.Context, campaignID string, now time.Time) (*State, error) {
	facets := []string{models.IdeaFacetOrigin, models.IdeaFacetSourceAsset}
	if campaignID == "" {
		facets = append(facets, models.IdeaFacetCampaign)
	}
	res, err := s.Query(ctx, models.IdeaQuery{
		ScopeCampaign: campaignID, Pile: models.AssetValueFilter{Any: []string{models.IdeaPileWaiting}},
		Facets: facets, Now: now, Location: time.UTC, Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	st := &State{CampaignID: campaignID, Total: res.TotalUnfiltered, Piles: res.Piles}
	for _, f := range res.Facets[models.IdeaFacetOrigin] {
		if f.Count > 0 {
			st.WaitingByOrigin = append(st.WaitingByOrigin, f)
		}
	}
	titles := map[string]string{}
	for _, a := range res.SourceAssetLabels {
		titles[a.ID] = a.Title
	}
	for _, f := range res.Facets[models.IdeaFacetSourceAsset] {
		if f.Value != models.AssetFilterNone && f.Count > 0 && len(st.WaitingBySource) < StateTop {
			st.WaitingBySource = append(st.WaitingBySource, SourceCount{AssetID: f.Value, Title: titles[f.Value], Count: f.Count})
		}
	}
	// The backlog comes last in the facet; here it ranks by its count.
	byCampaign := slices.DeleteFunc(slices.Clone(res.Facets[models.IdeaFacetCampaign]), func(f models.AssetFacetCount) bool { return f.Count == 0 })
	slices.SortStableFunc(byCampaign, func(a, b models.AssetFacetCount) int { return cmp.Compare(b.Count, a.Count) })
	st.WaitingByCampaign = byCampaign[:min(len(byCampaign), StateTop)]
	if st.Planned, st.PlannedTotal, err = s.repo.PlannedIdeas(ctx, campaignID, StatePlanned); err != nil {
		return nil, err
	}
	return st, nil
}
