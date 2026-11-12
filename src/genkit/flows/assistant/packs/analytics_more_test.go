package packs

import (
	"context"
	"database/sql"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/more_like_these"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/ideas"
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
		out[id] = repository.LensDigest{ID: "an-" + id, Status: models.AssetAnalysisStatusReady, Gist: "what " + id + " offers"}
	}
	return out, nil
}

// ListByAsset is the report's campaign lens; other assets have none.
func (gistAnalyses) ListByAsset(_ context.Context, id string) ([]models.AssetAnalysis, error) {
	if id != "a-report" {
		return nil, nil
	}
	return []models.AssetAnalysis{{ID: "an-other", AssetID: id}, {ID: "an-report", AssetID: id, CampaignID: new("c1"), CampaignName: new("Spring")}}, nil
}

// postFigures has figures for the first example only.
type postFigures struct {
	repository.PostAnalyticsRepository
}

func (postFigures) GetByPostID(_ context.Context, id string) (*models.PostAnalytics, error) {
	if id != "top1" {
		return nil, nil
	}
	return &models.PostAnalytics{PostID: id, EngagementRate: 4.2}, nil
}

// moreDeps is two example posts — one made from an idea read off an asset
// and using another, one with no trail — in a campaign with one more source.
func moreDeps(written []more_like_these.Idea) (Deps, *memIdeaRepo, *more_like_these.Request) {
	ideasRepo := &memIdeaRepo{byID: map[string]*models.Idea{
		"idea-pricing": {ID: "idea-pricing", Title: "Pricing myths", Angle: new("Discounts teach buyers to wait"), SourceAssetID: new("a-report"), AssetAnalysisID: new("an-report")},
	}}
	var got more_like_these.Request
	d := Deps{
		IdeaRepo: ideasRepo, Links: exampleLinks{linked: map[string][]string{}}, Analyses: gistAnalyses{},
		Analytics: &analytics.Service{Repo: postFigures{}},
		Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring", AssetIDs: models.StringSlice{"a-report", "a-deck"}}},
		Assets: memAssets{all: []models.Asset{
			{ID: "a-report", Title: "Pricing report", Status: "ready", Type: new("PDF")}, {ID: "a-deck", Title: "Sales deck", Status: "ready"}, {ID: "a-used", Title: "Survey", Status: "ready"},
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

// An asset cited before the idea it came through is still one source, traced
// to that idea and the analysis the idea came by, not the asset's digest.
func TestMoreLikeThese_AssetBeforeItsIdea(t *testing.T) {
	d, repo, _ := moreDeps([]more_like_these.Idea{{Title: "Survey says", Sources: []string{"asset:a-used", "idea:idea-pricing"}}})
	// The idea came from the asset the example used, by its own analysis.
	repo.byID["idea-pricing"].SourceAssetID, repo.byID["idea-pricing"].AssetAnalysisID = new("a-used"), new("an-survey")
	ctx, _ := turn(t, workspace, nil)
	out, err := moreLikeThese(ctx, d, MoreLikeTheseInput{PostIDs: []string{"top1"}, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if g := out.Ideas[0].Grounding; !slices.Equal(g.Sources, []assistantkit.Source{
		{ID: "a-used", Title: "Survey", AnalysisID: "an-survey", IdeaID: "idea-pricing"},
	}) {
		t.Fatalf("grounding = %+v", g)
	}
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
	if len(first.Ideas) != 1 || first.Ideas[0].Asset == nil || first.Ideas[0].Asset.Title != "Pricing report" || first.Ideas[0].Asset.Type != "PDF" ||
		len(first.Assets) != 1 || first.Assets[0].ID != "a-used" || first.NoTrail || first.Rate == nil || *first.Rate != 4.2 {
		t.Fatalf("trail = %+v", first)
	}
	// The idea names the campaign lens it was distilled by.
	if an := first.Ideas[0].Analysis; an == nil || *an != (assistantkit.TrailAnalysis{ID: "an-report", CampaignID: "c1", CampaignName: "Spring"}) {
		t.Fatalf("analysis = %+v", an)
	}
	if !out.Trail[1].NoTrail || out.Trail[1].Rate != nil {
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
	// Its grounding traces the report through the idea and the analysis it
	// came by, and the deck through the analysis the writer read.
	if g := out.Ideas[0].Grounding; g.Mode != assistantkit.Grounded || !slices.Equal(g.Sources, []assistantkit.Source{
		{ID: "a-report", Title: "Pricing report", AnalysisID: "an-report", IdeaID: "idea-pricing"},
		{ID: "a-deck", Title: "Sales deck", AnalysisID: "an-a-deck"},
	}) {
		t.Fatalf("grounding = %+v", g)
	}
	// An idea citing nothing cites the examples, by kind, and isn't grounded.
	bare := repo.byID[out.Ideas[1].ID]
	if !slices.Equal(bare.Sources, []string{"Post: Why discounts backfire", "Post: The 3 pricing pages"}) || *bare.GroundingMode != models.GroundingExploratory {
		t.Fatalf("bare = %+v", bare)
	}
	if g := out.Ideas[1].Grounding; g.Mode != assistantkit.Exploratory || !slices.Equal(g.Sources, []assistantkit.Source{
		{ID: "top1", Title: "Why discounts backfire", Kind: assistantkit.SourcePost},
		{ID: "top2", Title: "The 3 pricing pages", Kind: assistantkit.SourcePost},
	}) {
		t.Fatalf("bare grounding = %+v", g)
	}
	if len(touched()) != 2 {
		t.Fatalf("touched = %v", touched())
	}
	// The result opens on the Ideas list filtered to what it saved.
	v, err := url.ParseQuery(out.Filter)
	if err != nil || v.Get("origin") != "assistant" || v.Get("campaign_id") != "c1" || v.Get("created_after") == "" || v.Get("created_before") <= v.Get("created_after") {
		t.Fatalf("filter = %q", out.Filter)
	}
	if _, err := ideas.ParseQuery(v, time.Now()); err != nil {
		t.Fatalf("the list refuses the filter: %v", err)
	}
}

// The offer shows the trail before the yes, with its verb; an example it
// can't read leaves the card without a trail.
func TestMoreLikeTheseCard(t *testing.T) {
	d, _, _ := moreDeps(nil)
	ctx, _ := turn(t, workspace, nil)
	card := moreLikeTheseCard(ctx, d, MoreLikeTheseInput{PostIDs: []string{"top1", "top2"}, Count: 4})
	if card.Verb != "Plan 4 ideas" || card.Meta != "like 2 posts" || len(card.Trail) != 2 || card.Trail[0].Ideas[0].Asset.ID != "a-report" || !card.Trail[1].NoTrail {
		t.Fatalf("card = %+v", card)
	}
	card = moreLikeTheseCard(ctx, d, MoreLikeTheseInput{PostIDs: []string{"top1", "gone"}})
	if card.Verb != "Plan 3 ideas" || card.Trail != nil {
		t.Fatalf("unreadable = %+v", card)
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
