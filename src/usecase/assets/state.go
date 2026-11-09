package assets

import (
	"context"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// StateNames is how many assets the state names per list.
const StateNames = 3

// StateReaders are what State reads; the asset and analysis repositories.
type StateReaders struct {
	Assets interface {
		Query(ctx context.Context, q models.AssetQuery) (*models.AssetQueryResult, error)
	}
	Analyses interface {
		State(ctx context.Context, campaignID *string, now time.Time, names int) (*repository.AnalysisState, error)
	}
}

// State is the asset bank of one scope at a glance: the workspace's, or one
// campaign's sources. The assistant reads it on the Assets screens, and the
// screens' suggestions read the same counts.
type State struct {
	CampaignID string `json:"campaign_id,omitempty"`
	// Total counts the assets in scope; Processing those still being read
	// in, ProcessingFailed those that couldn't be.
	Total            int `json:"total"`
	Processing       int `json:"processing"`
	ProcessingFailed int `json:"processing_failed"`
	// The analyses of the scope's lens, by status: done, waiting in the
	// queue, running now, failed (retryable) and skipped.
	Analysed       int `json:"analysed"`
	Waiting        int `json:"waiting"`
	Analysing      int `json:"analysing"`
	AnalysisFailed int `json:"analysis_failed"`
	Skipped        int `json:"skipped"`
	// WaitingAssets and FailedAssets name the first of them.
	WaitingAssets []AssetName `json:"waiting_assets"`
	FailedAssets  []AssetName `json:"failed_assets"`
	// Undecided is the undecided ideas per lens, the workspace lens first.
	Undecided []LensIdeas `json:"undecided"`
}

// AssetName names an asset.
type AssetName struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// LensIdeas is how many undecided ideas one lens holds; CampaignID is empty
// for the workspace lens.
type LensIdeas struct {
	CampaignID   string `json:"campaign_id,omitempty"`
	CampaignName string `json:"campaign_name,omitempty"`
	Ideas        int    `json:"ideas"`
}

// UndecidedTotal sums the undecided ideas of every lens.
func (s *State) UndecidedTotal() int {
	n := 0
	for _, l := range s.Undecided {
		n += l.Ideas
	}
	return n
}

// ReadState reads the state of the workspace's bank, or of campaignID's
// sources. sql.ErrNoRows when campaignID names no live campaign.
func ReadState(ctx context.Context, r StateReaders, campaignID string, now time.Time) (*State, error) {
	res, err := r.Assets.Query(ctx, models.AssetQuery{
		ScopeCampaign: campaignID, Facets: []string{models.AssetFacetStatus}, Limit: 1,
		Sort: models.AssetSortUpdatedDesc, Now: now, Location: time.UTC,
	})
	if err != nil {
		return nil, err
	}
	var lens *string
	if campaignID != "" {
		lens = &campaignID
	}
	an, err := r.Analyses.State(ctx, lens, now, StateNames)
	if err != nil {
		return nil, err
	}
	s := &State{
		CampaignID: campaignID, Total: res.TotalUnfiltered,
		Analysed:       an.Lenses[models.AssetAnalysisStatusReady],
		Waiting:        an.Lenses[models.AssetAnalysisStatusPending],
		Analysing:      an.Lenses[models.AssetAnalysisStatusRunning],
		AnalysisFailed: an.Lenses[models.AssetAnalysisStatusFailed],
		Skipped:        an.Lenses[models.AssetAnalysisStatusSkipped],
		WaitingAssets:  names(an.Waiting),
		FailedAssets:   names(an.Failed),
		Undecided:      make([]LensIdeas, len(an.Undecided)),
	}
	for _, f := range res.Facets[models.AssetFacetStatus] {
		switch f.Value {
		case models.AssetStatusPending:
			s.Processing += f.Count
		case models.AssetStatusFailed:
			s.ProcessingFailed += f.Count
		}
	}
	for i, l := range an.Undecided {
		s.Undecided[i] = LensIdeas{CampaignName: l.CampaignName, Ideas: l.Ideas}
		if l.CampaignID != nil {
			s.Undecided[i].CampaignID = *l.CampaignID
		}
	}
	return s, nil
}

func names(ts []repository.AssetTitle) []AssetName {
	out := make([]AssetName, len(ts))
	for i, t := range ts {
		out[i] = AssetName{ID: t.ID, Title: t.Title}
	}
	return out
}
