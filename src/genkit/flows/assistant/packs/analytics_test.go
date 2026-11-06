package packs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

type examplePosts struct {
	repository.PostRepository
	byID map[string]*models.Post
}

func (r examplePosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	if p, ok := r.byID[id]; ok {
		return p, nil
	}
	return nil, sql.ErrNoRows
}

type exampleLinks struct {
	repository.PostIdeaRepository
	linked map[string][]string
}

func (exampleLinks) IdeasOfPost(_ context.Context, postID string) ([]models.PostSourceIdea, error) {
	if postID == "top1" {
		return []models.PostSourceIdea{{ID: "idea-pricing", Title: "Pricing myths"}}, nil
	}
	return nil, nil
}

func (l exampleLinks) Link(_ context.Context, _ bun.IDB, postID string, ids []string) error {
	l.linked[postID] = ids
	return nil
}

func TestMoreLikeThese_WritesFromTheExamplesAndKeepsProvenance(t *testing.T) {
	var got compose.DraftRequest
	links := exampleLinks{linked: map[string][]string{}}
	svc := compose.New(nil, nil, nil, links, nil, nil).WithDrafter(func(_ context.Context, req compose.DraftRequest) ([]compose.Written, []string, error) {
		got = req
		return []compose.Written{{PostID: "new1", PlatformID: req.PlatformID}}, nil, nil
	})
	d := Deps{Compose: svc, Links: links, DraftMax: 5, Posts: examplePosts{byID: map[string]*models.Post{
		"top1": {ID: "top1", CampaignID: "c1", PlatformID: "li", Title: "Why discounts backfire", Content: "Most buyers…", UsedAssetIDs: models.StringSlice{"a1"}},
		"top2": {ID: "top2", CampaignID: "c1", PlatformID: "li", Title: "The 3 pricing pages", Content: "We tested…"},
	}}}
	ctx, touched := turn(t, workspace, nil)

	out, err := moreLikeThese(ctx, d, MoreLikeTheseInput{PostIDs: []string{"top1", "top2"}, Count: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got.CampaignID != "c1" || got.PlatformID != "li" || got.Count != 5 || !slices.Equal(got.UsedAssetIDs, []string{"a1"}) {
		t.Fatalf("request = %+v; want the examples' campaign and platform, the count capped", got)
	}
	// The writer requires a publish window; none was asked for, so one is set.
	start, errS := time.Parse(time.DateOnly, got.WindowStart)
	end, errE := time.Parse(time.DateOnly, got.WindowEnd)
	if errS != nil || errE != nil || end.Before(start) {
		t.Fatalf("window = %q..%q; want valid dates", got.WindowStart, got.WindowEnd)
	}
	if !strings.Contains(got.SourceMaterial, "Why discounts backfire") || !strings.Contains(got.SourceMaterial, "never repeat") ||
		!strings.Contains(got.SourceMaterial, "Pricing myths") {
		t.Fatalf("source material = %q", got.SourceMaterial)
	}
	// The new post links to the same ideas, its provenance.
	if len(out.Posts) != 1 || !slices.Equal(links.linked["new1"], []string{"idea-pricing"}) {
		t.Fatalf("out = %+v, links = %v", out, links.linked)
	}
	if len(touched()) != 1 {
		t.Fatalf("touched = %v", touched())
	}
	_, err = moreLikeThese(ctx, d, MoreLikeTheseInput{PostIDs: []string{"nope"}})
	refused(t, err)
	refused(t, checkExamples(MoreLikeTheseInput{}))
}

func TestMoreLikeThese_KeepsAPartialDraft(t *testing.T) {
	links := exampleLinks{linked: map[string][]string{}}
	svc := compose.New(nil, nil, nil, links, nil, nil).WithDrafter(func(_ context.Context, req compose.DraftRequest) ([]compose.Written, []string, error) {
		return []compose.Written{{PostID: "new1", PlatformID: req.PlatformID}}, nil, errors.New("writer down")
	})
	d := Deps{Compose: svc, Links: links, DraftMax: 5, Posts: examplePosts{byID: map[string]*models.Post{
		"top1": {ID: "top1", CampaignID: "c1", PlatformID: "li", Title: "Why discounts backfire", Content: "Most buyers…"},
	}}}
	ctx, touched := turn(t, workspace, nil)

	out, err := moreLikeThese(ctx, d, MoreLikeTheseInput{PostIDs: []string{"top1"}, Count: 2})
	if err != nil {
		t.Fatalf("a partial draft is a result, not an error: %v", err)
	}
	if len(out.Posts) != 1 || out.Posts[0].PostID != "new1" || !slices.Equal(links.linked["new1"], []string{"idea-pricing"}) {
		t.Fatalf("out = %+v, links = %v; want the written post and its provenance", out, links.linked)
	}
	if len(touched()) != 1 {
		t.Fatalf("touched = %v", touched())
	}
	if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.HasPrefix(w, "stopped early: ") }) {
		t.Fatalf("warnings = %v; want the stopped-early warning", out.Warnings)
	}
}

type slugPlatforms struct{ repository.PlatformRepository }

func (slugPlatforms) List(context.Context) ([]models.Platform, error) {
	return []models.Platform{{ID: "p-li", ZernioID: "linkedin"}, {ID: "p-ig", ZernioID: "instagram"}}, nil
}

func TestRangeAndScope_InheritsTheScreenWindow(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{Platforms: slugPlatforms{}}}
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	ctx, _ = assistant.NewViewTurnContext(ctx, viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "analytics", Scope: workspace,
		Window: &viewcontext.Window{From: "2026-10-01", To: "2026-10-14", Platforms: []string{"linkedin"}},
	}, nil)

	// Nothing asked: the screen's dates and platforms.
	rng, sc, err := rangeAndScope(ctx, d, AnalyticsQuery{}, "")
	if err != nil || rng.Days != 14 || rng.From.Format(time.DateOnly) != "2026-10-01" || !sc.AllowsSlug("linkedin") || sc.AllowsSlug("instagram") {
		t.Fatalf("range = %+v, scope = %+v, %v; want the screen window", rng, sc, err)
	}
	// Other dates: those dates, still the screen's platform.
	rng, sc, err = rangeAndScope(ctx, d, AnalyticsQuery{From: "2026-11-01", To: "2026-11-07"}, "")
	if err != nil || rng.Days != 7 || !sc.AllowsSlug("linkedin") || sc.AllowsSlug("instagram") {
		t.Fatalf("range = %+v, scope = %+v, %v; want the asked dates on the screen's platform", rng, sc, err)
	}
	// A platform asked for replaces the screen's.
	_, sc, err = rangeAndScope(ctx, d, AnalyticsQuery{Window: "7d", Platforms: []string{"instagram"}}, "")
	if err != nil || !sc.AllowsSlug("instagram") || sc.AllowsSlug("linkedin") {
		t.Fatalf("scope = %+v, %v; want the asked platform", sc, err)
	}
}

func TestAnalyticsReads_RangeAndDegrade(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{}}
	// A from/to pair is the range.
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	ctx, _ = assistant.NewTurnContext(ctx, workspace, nil)
	rng, _, err := rangeAndScope(ctx, d, AnalyticsQuery{From: "2026-11-01", To: "2026-11-07"}, "")
	if err != nil || rng.Days != 7 {
		t.Fatalf("range = %+v, %v", rng, err)
	}
	_, _, err = rangeAndScope(ctx, d, AnalyticsQuery{Window: "forever"}, "")
	refused(t, err)

	// No analytics database: an honest "not configured", not an error.
	out, err := analyticsOverview(ctx, d, AnalyticsQuery{})
	if u, ok := out.(Unavailable); err != nil || !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("overview = %#v, %v", out, err)
	}
	_, err = topPosts(ctx, d, TopPostsInput{By: "vibes"})
	refused(t, err)
}

func TestAnalyticsLessons_RefusesAndDegrades(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{}}
	ctx, _ := turn(t, workspace, nil)
	for _, in := range []LessonsInput{{Metric: "vibes"}, {Since: "last spring"}, {TrendWindow: "forever"}} {
		_, err := analyticsLessons(ctx, d, in)
		refused(t, err)
	}
	out, err := analyticsLessons(ctx, d, LessonsInput{Since: "2026-01-01", TrendWindow: "12w", Metric: "saves"})
	if u, ok := out.(Unavailable); err != nil || !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("lessons = %#v, %v", out, err)
	}
}

type noAnalytics struct {
	repository.PostAnalyticsRepository
}

func TestPostStats_DefaultsToThePostThread(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	// Outside a post thread the id is required.
	_, err := postStats(ctx, Deps{Analytics: &analytics.Service{}}, PostStatsInput{})
	refused(t, err)

	postThread, _ := turn(t, viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}, nil)
	out, err := postStats(postThread, Deps{Analytics: &analytics.Service{}}, PostStatsInput{})
	if u, ok := out.(Unavailable); err != nil || !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("post stats = %#v, %v", out, err)
	}

	d := Deps{Analytics: &analytics.Service{Repo: noAnalytics{}, Posts: examplePosts{byID: map[string]*models.Post{
		"draft": {ID: "draft"},
	}}}}
	_, err = postStats(postThread, d, PostStatsInput{})
	refused(t, err)
	_, err = postStats(ctx, d, PostStatsInput{PostID: "draft"})
	refused(t, err)
}

type memSettings struct{ repository.SettingRepository }

func (memSettings) GetByKey(context.Context, string) (*models.Setting, error) {
	return &models.Setting{Key: "timezone", Value: "Europe/Kyiv"}, nil
}

type memAccounts struct {
	repository.SocialAccountRepository
}

func (memAccounts) ListActive(context.Context, string) ([]models.SocialAccount, error) {
	yes := true
	return []models.SocialAccount{{ID: "sa1", Platform: "linkedin", Username: "acme", NeedsReconnect: &yes}}, nil
}

func TestWorkspaceSetup(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	out, err := workspaceSetup(ctx, Deps{Settings: memSettings{}, SocialAccounts: memAccounts{},
		ProfileID: func(context.Context) (string, error) { return "prof", nil }})
	if err != nil || out.Timezone != "Europe/Kyiv" || len(out.Accounts) != 1 || !out.Accounts[0].NeedsReconnect {
		t.Fatalf("setup = %+v, %v", out, err)
	}
	out, err = workspaceSetup(ctx, Deps{Settings: memSettings{}})
	if err != nil || out.Note == "" {
		t.Fatalf("no accounts store = %+v, %v", out, err)
	}
	out, err = workspaceSetup(ctx, Deps{Settings: memSettings{}, SocialAccounts: memAccounts{},
		ProfileID: func(context.Context) (string, error) { return "", nil }})
	if err != nil || out.Note == "" {
		t.Fatalf("no profile = %+v, %v; want a note", out, err)
	}
	// A failed profile read is the failure, not "no profile connected".
	down := errors.New("settings unavailable")
	if _, err = workspaceSetup(ctx, Deps{Settings: memSettings{}, SocialAccounts: memAccounts{},
		ProfileID: func(context.Context) (string, error) { return "", down }}); !errors.Is(err, down) {
		t.Fatalf("profile lookup failure = %v; want it returned", err)
	}
}
