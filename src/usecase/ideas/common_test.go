package ideas

import (
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
)

// What two or more ideas share is named with the ideas that share it, the
// most shared first; what only one has is left out.
func TestInCommon(t *testing.T) {
	c1 := "c1"
	list := []models.Idea{
		{ID: "i1", Title: "Pricing myths founders believe", CampaignID: &c1, Origin: models.IdeaOriginAssetAnalysis, SourceAssetID: new("a1"),
			FitBreakdown: &ideafit.Breakdown{Lens: ideafit.LensWorkspace}, Plan: models.IdeaPlan{PlatformID: new("linkedin")}},
		{ID: "i2", Title: "Why pricing pages fail", CampaignID: &c1, Origin: models.IdeaOriginAssetAnalysis, AssetIDs: []string{"a1", "a2"},
			FitBreakdown: &ideafit.Breakdown{Lens: ideafit.LensWorkspace}, Plan: models.IdeaPlan{PlatformID: new("linkedin")}},
		{ID: "i3", Title: "A founder's pricing story", Angle: new("told by the founder"), AssetIDs: []string{"a2"}},
	}
	c := InCommon(list)
	values := func(s []Shared) []string {
		var out []string
		for _, v := range s {
			out = append(out, v.Value)
		}
		return out
	}
	for name, got := range map[string][]string{
		"terms": values(c.Terms), "assets": values(c.Assets), "campaigns": values(c.Campaigns),
		"origins": values(c.Origins), "lenses": values(c.Lenses), "platforms": values(c.Platforms), "post types": values(c.PostTypes),
	} {
		want := map[string][]string{
			"terms": {"pricing", "founder"}, "assets": {"a1", "a2"}, "campaigns": {"c1"},
			"origins": {"asset_analysis"}, "lenses": {"workspace"}, "platforms": {"linkedin"}, "post types": nil,
		}[name]
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if !slices.Equal(c.Terms[0].IdeaIDs, []string{"i1", "i2", "i3"}) || !slices.Equal(c.Assets[1].IdeaIDs, []string{"i2", "i3"}) {
		t.Fatalf("ideas sharing = %+v / %+v", c.Terms[0], c.Assets[1])
	}
	// One platform among the ideas that have one is no split; nor is a
	// campaign held by some ideas while the rest have none.
	if len(c.Splits) != 0 {
		t.Fatalf("splits = %+v; want none", c.Splits)
	}

	list[2].Plan.PlatformID, list[2].Plan.PostType = new("instagram"), new("carousel")
	c = InCommon(list)
	if len(c.Splits) != 1 || c.Splits[0].Field != FieldPlatform || !slices.Equal(values(c.Splits[0].Values), []string{"linkedin", "instagram"}) ||
		!slices.Equal(c.Splits[0].Values[1].IdeaIDs, []string{"i3"}) {
		t.Fatalf("splits = %+v", c.Splits)
	}
}
