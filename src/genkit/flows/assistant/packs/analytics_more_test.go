package packs

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/more_like_these"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func (r *memIdeaRepo) Create(_ context.Context, i *models.Idea) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *i
	r.byID[i.ID] = &cp
	return nil
}

func (r memAssets) GetByID(_ context.Context, id string) (*models.Asset, error) {
	for _, a := range r.all {
		if a.ID == id {
			return &a, nil
		}
	}
	return nil, sql.ErrNoRows
}

type gistAnalyses struct {
	repository.AssetAnalysisRepository
}

func (gistAnalyses) LensDigests(_ context.Context, ids []string, _ *string) (map[string]repository.LensDigest, error) {
	out := map[string]repository.LensDigest{}
	for _, id := range ids {
		out[id] = repository.LensDigest{Gist: "what " + id + " offers"}
	}
	return out, nil
}

// moreDeps is two example posts — one made from an idea read off an asset
// and using another, one with no trail — in a campaign with one more source.
func moreDeps(written []more_like_these.Idea) (Deps, *memIdeaRepo, *more_like_these.Request) {
	ideasRepo := &memIdeaRepo{byID: map[string]*models.Idea{
		"idea-pricing": {ID: "idea-pricing", Title: "Pricing myths", Angle: new("Discounts teach buyers to wait"), SourceAssetID: new("a-report")},
	}}
	var got more_like_these.Request
	d := Deps{
		IdeaRepo: ideasRepo, Links: exampleLinks{linked: map[string][]string{}}, Analyses: gistAnalyses{},
		Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring", AssetIDs: models.StringSlice{"a-report", "a-deck"}}},
		Assets: memAssets{all: []models.Asset{
			{ID: "a-report", Title: "Pricing report", Status: "ready"}, {ID: "a-deck", Title: "Sales deck", Status: "ready"}, {ID: "a-used", Title: "Survey", Status: "ready"},
		}},
		Posts: examplePosts{byID: map[string]*models.Post{
			"top1": {ID: "top1", CampaignID: "c1", PlatformID: "li", Title: "Why discounts backfire", Content: "Most buyers…", UsedAssetIDs: models.StringSlice{"a-used"}},
			"top2": {ID: "top2", CampaignID: "c1", PlatformID: "li", Title: "The 3 pricing pages", Content: "We tested…"},
		}},
		MoreLike: func(_ context.Context, req more_like_these.Request) ([]more_like_these.Idea, error) {
			got = req
			return written, nil
		},
	}
	return d, ideasRepo, &got
}

// The trail follows each example to its ideas and their assets, and its
// used assets; the campaign's other sources ground the writer; the ideas
// are saved undated in the campaign, citing their sources by name.
func TestMoreLikeThese_WritesIdeasFromTheTrail(t *testing.T) {
	d, repo, got := moreDeps([]more_like_these.Idea{
		{Title: "Price anchoring", Angle: "Show the dearest plan first", Theses: []string{"anchors", "decoys"}, Sources: []string{"idea:idea-pricing", "asset:a-deck"}},
		{Title: "No sources cited"},
	})
	ctx, touched := turn(t, workspace, nil)

	out, err := moreLikeThese(ctx, d, MoreLikeTheseInput{PostIDs: []string{"top1", "top2"}, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := out.Trail[0]
	if len(first.Ideas) != 1 || first.Ideas[0].Asset == nil || first.Ideas[0].Asset.Title != "Pricing report" ||
		len(first.Assets) != 1 || first.Assets[0].ID != "a-used" || first.NoTrail {
		t.Fatalf("trail = %+v", first)
	}
	if !out.Trail[1].NoTrail {
		t.Fatalf("an example with nothing recorded = %+v", out.Trail[1])
	}
	// The report is on the trail already; the deck grounds besides it.
	if len(out.Grounding) != 1 || out.Grounding[0].ID != "a-deck" || out.CampaignID != "c1" {
		t.Fatalf("grounding = %+v", out.Grounding)
	}
	refs := map[string]string{}
	for _, s := range got.Sources {
		refs[s.Ref] = s.Text
	}
	if refs["idea:idea-pricing"] != "Discounts teach buyers to wait" || refs["asset:a-deck"] != "what a-deck offers" || got.Count != 2 || len(got.Examples) != 2 {
		t.Fatalf("request = %+v", got)
	}

	if len(out.Ideas) != 2 {
		t.Fatalf("ideas = %+v", out.Ideas)
	}
	saved := repo.byID[out.Ideas[0].ID]
	if saved.Origin != models.IdeaOriginAssistant || saved.Plan.TargetDate != nil || *saved.CampaignID != "c1" || *saved.Plan.PlatformID != "li" ||
		!slices.Equal(saved.Sources, []string{"Idea: Pricing myths", "Asset: Sales deck"}) ||
		!slices.Equal(saved.AssetIDs, []string{"a-report", "a-deck"}) || *saved.GroundingMode != models.GroundingGrounded ||
		saved.Note != "- anchors\n- decoys" || *saved.Angle != "Show the dearest plan first" {
		t.Fatalf("saved = %+v", saved)
	}
	// An idea citing nothing cites the examples, and isn't grounded.
	bare := repo.byID[out.Ideas[1].ID]
	if !slices.Equal(bare.Sources, []string{"Post: Why discounts backfire", "Post: The 3 pricing pages"}) || *bare.GroundingMode != models.GroundingExploratory {
		t.Fatalf("bare = %+v", bare)
	}
	if len(touched()) != 2 {
		t.Fatalf("touched = %v", touched())
	}
}

// With no posts named, the posts selected on screen are the examples; none
// at all is refused.
func TestMoreLikeThese_SelectedPosts(t *testing.T) {
	d, _, got := moreDeps([]more_like_these.Idea{{Title: "One"}})
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	sel, _ := assistant.NewViewTurnContext(ctx, viewcontext.ViewContext{Version: viewcontext.Version, Screen: "analytics", Scope: workspace,
		Selection: &viewcontext.Selection{Kind: viewcontext.KindPost, IDs: []string{"top2"}}}, nil)
	if _, err := moreLikeThese(sel, d, MoreLikeTheseInput{}); err != nil || len(got.Examples) != 1 || got.Examples[0].ID != "top2" {
		t.Fatalf("examples = %+v, %v", got.Examples, err)
	}
	none, _ := assistant.NewTurnContext(ctx, workspace, nil)
	_, err := moreLikeThese(none, d, MoreLikeTheseInput{})
	refused(t, err)
	_, err = moreLikeThese(none, d, MoreLikeTheseInput{PostIDs: []string{"nope"}})
	refused(t, err)
}
