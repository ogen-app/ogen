package ideas

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// What makes ideas similar beyond their words: the same source asset, an
// asset in common, or the same lens's campaign. A pair below
// SimilarThreshold, boosts included, isn't similar.
const (
	SimilarThreshold  = 0.2
	sameSourceBoost   = 0.1
	sharedAssetBoost  = 0.05
	sameCampaignBoost = 0.05
	MaxSimilar        = 20
)

// SimilarIdea is an idea like another, how alike (0–1) and why.
type SimilarIdea struct {
	Idea  models.Idea
	Score float64
	// Terms are the words they share; SameSource, SharedAssets and
	// SameCampaign the other ties.
	Terms        []string
	SameSource   bool
	SharedAssets []string
	SameCampaign bool
}

// Similar returns up to limit ideas like the idea id, the most alike first,
// from the newest MaxDuplicateScan in scope (a campaign's, or with
// campaignID empty every idea), turned-down ones left out. It scores words
// as the duplicate search does, with a lower bar, and adds the ties the
// words miss.
func (s *Service) Similar(ctx context.Context, id, campaignID string, limit int) ([]SimilarIdea, error) {
	idea, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := s.Query(ctx, models.IdeaQuery{
		ScopeCampaign: campaignID, Pile: models.AssetValueFilter{Not: []string{models.IdeaPileNo}},
		Sort: models.IdeaSortCreatedDesc, Limit: MaxDuplicateScan, Now: s.now(), Location: time.UTC,
	})
	if err != nil {
		return nil, err
	}
	return similarTo(*idea, res.Ideas, max(1, min(limit, MaxSimilar))), nil
}

// similarTo scores every candidate against idea and keeps the best limit.
func similarTo(idea models.Idea, candidates []models.Idea, limit int) []SimilarIdea {
	base := similarTokens(idea)
	var out []SimilarIdea
	for _, c := range candidates {
		if c.ID == idea.ID {
			continue
		}
		t := similarTokens(c)
		sim := SimilarIdea{Idea: c, Score: similarity(base, t), Terms: intersect(base.all, t.all)}
		if idea.SourceAssetID != nil && c.SourceAssetID != nil && *idea.SourceAssetID == *c.SourceAssetID {
			sim.SameSource = true
			sim.Score += sameSourceBoost
		}
		if sim.SharedAssets = intersect(assetsOf(idea), assetsOf(c)); len(sim.SharedAssets) > 0 && !sim.SameSource {
			sim.Score += sharedAssetBoost
		}
		if idea.CampaignID != nil && c.CampaignID != nil && *idea.CampaignID == *c.CampaignID {
			sim.SameCampaign = true
			sim.Score += sameCampaignBoost
		}
		sim.Score = math.Round(min(sim.Score, 1)*100) / 100
		if sim.Score >= SimilarThreshold && (len(sim.Terms) > 0 || sim.SameSource || len(sim.SharedAssets) > 0) {
			out = append(out, sim)
		}
	}
	slices.SortStableFunc(out, func(a, b SimilarIdea) int { return cmp.Compare(b.Score, a.Score) })
	return out[:min(len(out), limit)]
}

// similarTokens is an idea's words for similarity: its title, and its
// title with its note and angle.
func similarTokens(i models.Idea) ideaTokens {
	title := tokenize(i.Title)
	all := union(title, tokenize(i.Note))
	if i.Angle != nil {
		all = union(all, tokenize(*i.Angle))
	}
	return ideaTokens{title: title, all: all}
}

// assetsOf is the sorted assets an idea came from or drew on.
func assetsOf(i models.Idea) []string {
	ids := slices.Clone(i.AssetIDs)
	if i.SourceAssetID != nil {
		ids = append(ids, *i.SourceAssetID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// intersect is what two sorted, unique lists share.
func intersect(a, b []string) []string {
	var out []string
	for _, v := range a {
		if _, ok := slices.BinarySearch(b, v); ok {
			out = append(out, v)
		}
	}
	return out
}
