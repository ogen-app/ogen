package packs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type ideaPosts struct{ repository.PostIdeaRepository }

func (ideaPosts) PostsOfIdea(_ context.Context, id string) ([]models.IdeaPostRef, error) {
	if id == "i1" {
		return []models.IdeaPostRef{{ID: "p1", Title: "Pricing myths, debunked", Status: models.PostStatusDraft}}, nil
	}
	return []models.IdeaPostRef{}, nil
}

func readDeps() Deps {
	d, repo, _ := ideaDeps()
	c1, an := "c1", "an1"
	repo.byID["i1"] = &models.Idea{ID: "i1", Title: "Pricing myths founders believe", Origin: models.IdeaOriginAssetAnalysis,
		CampaignID: &c1, Angle: new("myth-busting"), Rationale: new("the brochure lists three myths"), Sources: []string{"“Most founders underprice.”"},
		SourceAssetID: new("a1"), AssetAnalysisID: &an, FitScore: new(72),
		SourceLocator: &models.SourceLocator{ChunkID: "a1:4", Label: "p. 12", Page: new(12)},
		FitBreakdown: &ideafit.Breakdown{Lens: ideafit.LensCampaign, CampaignID: &c1,
			Criteria: map[string]ideafit.Rating{"evidence": {Level: 4, Why: "a clear claim backs it"}}, Caps: []ideafit.Cap{ideafit.CapOffBrief}}}
	repo.byID["i2"].CreatedByName, repo.byID["i2"].CampaignID, repo.byID["i2"].AssetIDs = "Olena", &c1, []string{"a1"}
	d.Assets = memAssets{all: []models.Asset{{ID: "a1", Title: "Brochure"}}}
	d.Analyses = lensRepo{lenses: []models.AssetAnalysis{{ID: "an1", AssetID: "a1", CampaignID: &c1, Status: models.AssetAnalysisStatusReady,
		Gist: "Pricing advice for founders.", Summary: strings.Repeat("s", ideaAnalysisRunes+10)}}}
	d.Links = ideaPosts{}
	d.Campaigns = &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring reopening"}}
	return d
}

// getIdea explains an idea: its parts, each rating with what its level
// means, the cap, its source with the passage it came from and analysis, and
// the posts made from it. A hand-written idea says no lens scored it.
func TestGetIdea(t *testing.T) {
	d := readDeps()
	ctx, _ := turn(t, workspace, nil)
	out, err := getIdea(ctx, d, GetIdeaInput{IdeaID: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Angle != "myth-busting" || out.Rationale == "" || len(out.Sources) != 1 || out.SourceAsset == nil || out.SourceAsset.Title != "Brochure" {
		t.Fatalf("idea = %+v", out)
	}
	if p := out.SourceAsset.Passage; p.ChunkID != "a1:4" || p.Label != "p. 12" || p.Page == nil || *p.Page != 12 {
		t.Fatalf("passage = %+v", p)
	}
	f := out.Fit
	if f == nil || *f.Score != 72 || f.Lens != "campaign" || f.CampaignID != "c1" || len(f.Criteria) != 1 ||
		f.Criteria[0].Means != "a clear claim or example from the source supports it" || f.Criteria[0].Question == "" || len(f.Caps) != 1 || f.Caps[0].Meaning == "" {
		t.Fatalf("fit = %+v", f)
	}
	if a := out.Analysis; a == nil || a.ID != "an1" || a.Lens.CampaignID != "c1" || len([]rune(a.Summary)) != ideaAnalysisRunes {
		t.Fatalf("analysis = %+v", a)
	}
	if len(out.Posts) != 1 || out.Posts[0].ID != "p1" || out.Note2 != "" {
		t.Fatalf("posts = %+v, explain %q", out.Posts, out.Note2)
	}

	hand, err := getIdea(ctx, d, GetIdeaInput{IdeaID: "i2"})
	if err != nil || hand.Fit != nil || hand.Note2 == "" || hand.Author != "Olena" || hand.Posts == nil {
		t.Fatalf("hand-written = %+v, %v", hand, err)
	}
	_, err = getIdea(ctx, d, GetIdeaInput{})
	refused(t, err)
	_, err = getIdea(ctx, d, GetIdeaInput{IdeaID: "gone"})
	refused(t, err)
}

// ideasInCommon names what two or more ideas share, labelled; an idea that
// is gone is reported, and fewer than two is refused.
func TestIdeasInCommon(t *testing.T) {
	d := readDeps()
	ctx, _ := turn(t, workspace, nil)
	out, err := ideasInCommon(ctx, d, IdeasInCommonInput{IdeaIDs: []string{"i1", "i2", "gone"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Ideas) != 2 || !slices.Equal(out.Missing, []string{"gone"}) {
		t.Fatalf("ideas = %+v, missing %v", out.Ideas, out.Missing)
	}
	if len(out.Assets) != 1 || out.Assets[0].Label != "Brochure" || len(out.Campaigns) != 1 || out.Campaigns[0].Label != "Spring reopening" {
		t.Fatalf("shared = %+v / %+v", out.Assets, out.Campaigns)
	}
	if len(out.Terms) != 1 || out.Terms[0].Value != "founder" {
		t.Fatalf("terms = %+v; founders and Founder share a word", out.Terms)
	}
	for _, in := range [][]string{{"i1"}, {"i1", "gone"}} {
		_, err := ideasInCommon(ctx, d, IdeasInCommonInput{IdeaIDs: in})
		refused(t, err)
	}
}
