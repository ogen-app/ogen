package ideas

import (
	"cmp"
	"slices"

	"github.com/ogen-app/ogen/src/domain/models"
)

// maxCommonTerms caps the shared words InCommon names.
const maxCommonTerms = 10

// Shared is one value at least two of a set's ideas have, and which ideas.
type Shared struct {
	Value   string
	IdeaIDs []string
}

// Common is what a set of ideas share: each list holds the values two or
// more of them have, the most shared first.
type Common struct {
	// Assets are the assets they came from or drew on.
	Assets    []Shared
	Campaigns []Shared
	Origins   []Shared
	// Lenses are the lenses that scored them: "workspace", or a campaign
	// lens's campaign id.
	Lenses    []Shared
	Platforms []Shared
	PostTypes []Shared
	Phases    []Shared
	// Terms are the words of their titles and angles, stopwords and plurals
	// dropped, maxCommonTerms at most.
	Terms []Shared
}

// InCommon works out what list's ideas share, in code: the model words it.
func InCommon(list []models.Idea) Common {
	var c Common
	field := func(get func(models.Idea) []string) []Shared {
		by := map[string][]string{}
		for _, i := range list {
			for _, v := range get(i) {
				by[v] = append(by[v], i.ID)
			}
		}
		var out []Shared
		for v, ids := range by {
			if len(ids) > 1 {
				out = append(out, Shared{Value: v, IdeaIDs: ids})
			}
		}
		slices.SortFunc(out, func(a, b Shared) int {
			return cmp.Or(cmp.Compare(len(b.IdeaIDs), len(a.IdeaIDs)), cmp.Compare(a.Value, b.Value))
		})
		return out
	}
	one := func(p *string) []string {
		if p == nil || *p == "" {
			return nil
		}
		return []string{*p}
	}
	c.Assets = field(func(i models.Idea) []string {
		ids := slices.Concat(one(i.SourceAssetID), i.AssetIDs)
		slices.Sort(ids)
		return slices.Compact(ids)
	})
	c.Campaigns = field(func(i models.Idea) []string { return one(i.CampaignID) })
	c.Origins = field(func(i models.Idea) []string {
		if i.Origin == "" {
			return []string{models.IdeaOriginKeyManual}
		}
		return []string{string(i.Origin)}
	})
	c.Lenses = field(func(i models.Idea) []string {
		switch b := i.FitBreakdown; {
		case b == nil:
			return nil
		case b.CampaignID != nil:
			return []string{*b.CampaignID}
		}
		return []string{"workspace"}
	})
	c.Platforms = field(func(i models.Idea) []string { return one(i.Plan.PlatformID) })
	c.PostTypes = field(func(i models.Idea) []string { return one(i.Plan.PostType) })
	c.Phases = field(func(i models.Idea) []string { return one(i.Plan.PhaseID) })
	terms := field(func(i models.Idea) []string {
		text := i.Title
		if i.Angle != nil {
			text += " " + *i.Angle
		}
		return tokenize(text)
	})
	c.Terms = terms[:min(len(terms), maxCommonTerms)]
	return c
}
