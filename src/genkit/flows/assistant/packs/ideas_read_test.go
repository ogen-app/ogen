package packs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/ideas_common"
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
// means, the cap, its source and analysis, and the posts made from it. A
// hand-written idea says no lens scored it.
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
	if out.Theme != "" || out.Angle != "" || len(out.Splits) != 0 {
		t.Fatalf("without a namer or a split = %+v", out)
	}
	for _, in := range [][]string{{"i1"}, {"i1", "gone"}} {
		_, err := ideasInCommon(ctx, d, IdeasInCommonInput{IdeaIDs: in})
		refused(t, err)
	}
}

// The thread the ideas share is worded by the namer, and where they split
// is named by field; a namer that fails leaves the rest of the result.
func TestIdeasInCommon_ThreadAndSplits(t *testing.T) {
	d := readDeps()
	repo := d.IdeaRepo.(*memIdeaRepo)
	repo.byID["i1"].Plan.PlatformID, repo.byID["i2"].Plan.PlatformID = new("linkedin"), new("instagram")
	var asked []ideas_common.Idea
	d.CommonThread = func(_ context.Context, req ideas_common.Request) (*ideas_common.Result, error) {
		asked = req.Ideas
		return &ideas_common.Result{Theme: "founder pricing", Angle: "Both take apart what founders get wrong about price."}, nil
	}
	ctx, _ := turn(t, workspace, nil)
	out, err := ideasInCommon(ctx, d, IdeasInCommonInput{IdeaIDs: []string{"i1", "i2"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Theme != "founder pricing" || out.Angle == "" || len(asked) != 2 || asked[0].Angle != "myth-busting" {
		t.Fatalf("thread = %q / %q, asked %+v", out.Theme, out.Angle, asked)
	}
	if len(out.Splits) != 1 || out.Splits[0].Field != "platform" || len(out.Splits[0].Values) != 2 {
		t.Fatalf("splits = %+v", out.Splits)
	}

	d.CommonThread = func(context.Context, ideas_common.Request) (*ideas_common.Result, error) {
		return nil, errors.New("model down")
	}
	out, err = ideasInCommon(ctx, d, IdeasInCommonInput{IdeaIDs: []string{"i1", "i2"}})
	if err != nil || out.Theme != "" || len(out.Splits) != 1 {
		t.Fatalf("with the namer down = %+v, %v", out, err)
	}
}
