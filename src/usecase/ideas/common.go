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

// Split is a field the ideas differ on: every value they have, the most
// held first, each with its ideas.
type Split struct {
	Field  string
	Values []Shared
}

// The fields Splits names.
const (
	FieldCampaign = "campaign"
	FieldLens     = "lens"
	FieldPlatform = "platform"
	FieldPostType = "postType"
	FieldPhase    = "phase"
)

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
	// Splits are the plan and placement fields two or more of them hold
	// different values of, in a fixed order: campaign, lens, platform,
	// post type, phase.
	Splits []Split
}

// InCommon works out what list's ideas share, in code: the model words it.
func InCommon(list []models.Idea) Common {
	var c Common
	values := func(get func(models.Idea) []string, least int) []Shared {
		by := map[string][]string{}
		for _, i := range list {
			for _, v := range get(i) {
				by[v] = append(by[v], i.ID)
			}
		}
		var out []Shared
		for v, ids := range by {
			if len(ids) >= least {
				out = append(out, Shared{Value: v, IdeaIDs: ids})
			}
		}
		slices.SortFunc(out, func(a, b Shared) int {
			return cmp.Or(cmp.Compare(len(b.IdeaIDs), len(a.IdeaIDs)), cmp.Compare(a.Value, b.Value))
		})
		return out
	}
	field := func(get func(models.Idea) []string) []Shared { return values(get, 2) }
	split := func(name string, get func(models.Idea) []string) {
		if all := values(get, 1); len(all) > 1 {
			c.Splits = append(c.Splits, Split{Field: name, Values: all})
		}
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
	campaign := func(i models.Idea) []string { return one(i.CampaignID) }
	c.Campaigns = field(campaign)
	c.Origins = field(func(i models.Idea) []string {
		if i.Origin == "" {
			return []string{models.IdeaOriginKeyManual}
		}
		return []string{string(i.Origin)}
	})
	lens := func(i models.Idea) []string {
		switch b := i.FitBreakdown; {
		case b == nil:
			return nil
		case b.CampaignID != nil:
			return []string{*b.CampaignID}
		}
		return []string{"workspace"}
	}
	platform := func(i models.Idea) []string { return one(i.Plan.PlatformID) }
	postType := func(i models.Idea) []string { return one(i.Plan.PostType) }
	phase := func(i models.Idea) []string { return one(i.Plan.PhaseID) }
	c.Lenses, c.Platforms, c.PostTypes, c.Phases = field(lens), field(platform), field(postType), field(phase)
	split(FieldCampaign, campaign)
	split(FieldLens, lens)
	split(FieldPlatform, platform)
	split(FieldPostType, postType)
	split(FieldPhase, phase)
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
