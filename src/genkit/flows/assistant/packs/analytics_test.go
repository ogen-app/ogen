package packs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
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
	if u, ok := out.Data.(Unavailable); err != nil || !ok || u.Reason != analytics.ReasonNotConfigured {
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
	if u, ok := out.Data.(Unavailable); err != nil || !ok || u.Reason != analytics.ReasonNotConfigured {
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
	if u, ok := out.Data.(Unavailable); err != nil || !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("post stats = %#v, %v", out, err)
	}

	d := Deps{Analytics: &analytics.Service{Repo: noAnalytics{}, Posts: examplePosts{byID: map[string]*models.Post{
		"draft": {ID: "draft"},
	}}}}
	_, err = postStats(postThread, d, PostStatsInput{})
	refused(t, err)
	_, err = postStats(ctx, d, PostStatsInput{PostID: "draft"})
	refused(t, err)

	// With analytics configured, the post read is the thread's.
	posts := &recordingPosts{}
	_, err = postStats(postThread, Deps{Analytics: &analytics.Service{Repo: noAnalytics{}, Posts: posts}}, PostStatsInput{})
	refused(t, err)
	if !slices.Equal(posts.asked, []string{"p1"}) {
		t.Fatalf("posts read = %v, want the thread's p1", posts.asked)
	}
}

type recordingPosts struct {
	repository.PostRepository
	asked []string
}

func (r *recordingPosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	r.asked = append(r.asked, id)
	return nil, sql.ErrNoRows
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
