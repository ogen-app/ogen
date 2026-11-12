package packs

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// analyticsTurn is a workspace thread on the analytics screen with window.
func analyticsTurn(t *testing.T, w *viewcontext.Window) context.Context {
	t.Helper()
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	ctx, _ = assistant.NewViewTurnContext(ctx, viewcontext.ViewContext{Version: viewcontext.Version, Screen: "analytics", Scope: workspace, Window: w}, nil)
	return ctx
}

// A read cites what narrowed it: the screen's dates, platforms and
// campaign, the campaign by name. "all" widens to the workspace.
func TestAnalyticsOverview_CitesTheWindow(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{Platforms: slugPlatforms{}, Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring"}}}}
	ctx := analyticsTurn(t, &viewcontext.Window{From: "2026-10-01", To: "2026-10-14", Platforms: []string{"linkedin"}, CampaignID: "c1"})

	out, err := analyticsOverview(ctx, d, AnalyticsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	a := out.Applied
	if a.From != "2026-10-01" || a.To != "2026-10-14" || len(a.Platforms) != 1 || a.Platforms[0] != "linkedin" ||
		a.Campaign == nil || a.Campaign.ID != "c1" || a.Campaign.Name != "Spring" {
		t.Fatalf("applied = %+v", a)
	}
	if u, ok := out.Data.(Unavailable); !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("data = %#v", out.Data)
	}
	if out, err = analyticsOverview(ctx, d, AnalyticsQuery{CampaignID: "all"}); err != nil || out.Applied.Campaign != nil {
		t.Fatalf("all = %+v, %v", out, err)
	}
	// Dates picked by hand have no preset; a span, or none at all, does.
	if a.Preset != "" {
		t.Fatalf("dates preset = %q", a.Preset)
	}
	if out, err = analyticsOverview(ctx, d, AnalyticsQuery{Window: "12w"}); err != nil || out.Applied.Preset != "12w" {
		t.Fatalf("span = %+v, %v", out, err)
	}
	if out, err = analyticsOverview(analyticsTurn(t, nil), d, AnalyticsQuery{}); err != nil || out.Applied.Preset != defaultPreset {
		t.Fatalf("default = %+v, %v", out, err)
	}
}

// A live insight read takes the screen's platform when it shows one, refuses
// an unknown one, and says the publisher isn't set up.
func TestInsightReads(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{Platforms: slugPlatforms{}}}
	ctx := analyticsTurn(t, &viewcontext.Window{Platforms: []string{"linkedin"}})
	out, err := insightRead(ctx, d, InsightInput{}, d.Analytics.BestTimes)
	if err != nil || len(out.Applied.Platforms) != 1 || out.Applied.Platforms[0] != "linkedin" || out.Applied.Note == "" {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if u, ok := out.Data.(Unavailable); !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("data = %#v", out.Data)
	}
	_, err = insightRead(ctx, d, InsightInput{Platform: "myspace"}, d.Analytics.ContentDecay)
	refused(t, err)
	_, err = insightRead(ctx, d, InsightInput{Source: "bogus"}, d.Analytics.PostingFrequency)
	refused(t, err)
}

type followerSnapshots struct {
	repository.FollowerStatsRepository
	got repository.FollowerSeriesOptions
}

func (f *followerSnapshots) Summary(context.Context, string) ([]repository.FollowerAccountSummary, error) {
	return []repository.FollowerAccountSummary{{SocialAccountID: "acc", CurrentFollowers: 120}}, nil
}

func (f *followerSnapshots) Series(_ context.Context, opt repository.FollowerSeriesOptions) ([]repository.FollowerSeriesPoint, error) {
	f.got = opt
	day := func(s string) time.Time { t, _ := time.Parse(time.DateOnly, s); return t }
	return []repository.FollowerSeriesPoint{
		{SocialAccountID: "acc", PointDate: day("2026-10-05"), Followers: 100},
		{SocialAccountID: "acc", PointDate: day("2026-10-06"), Followers: 110},
		{SocialAccountID: "acc", PointDate: day("2026-10-13"), Followers: 120},
	}, nil
}

// The follower history reads the window's days, inclusive, at the asked
// granularity.
func TestFollowerHistory(t *testing.T) {
	snaps := &followerSnapshots{}
	d := Deps{Analytics: &analytics.Service{Followers: snaps}}
	ctx := analyticsTurn(t, nil)
	out, err := followerHistory(ctx, d, FollowerHistoryInput{From: "2026-10-01", To: "2026-10-31", Granularity: "weekly"})
	if err != nil {
		t.Fatal(err)
	}
	h, ok := out.Data.(*analytics.FollowerHistory)
	if !ok || len(h.Series["acc"]) != 2 || h.Series["acc"][0].Followers != 110 || h.Granularity != "weekly" {
		t.Fatalf("data = %#v", out.Data)
	}
	if snaps.got.From.Format(time.DateOnly) != "2026-10-01" || snaps.got.To.Format(time.DateOnly) != "2026-10-31" || out.Applied.To != "2026-10-31" {
		t.Fatalf("read %+v, applied %+v", snaps.got, out.Applied)
	}
	_, err = followerHistory(ctx, d, FollowerHistoryInput{Granularity: "hourly"})
	refused(t, err)
}

type listedAnalytics struct {
	repository.PostAnalyticsRepository
	got repository.PostAnalyticsListOptions
}

func (l *listedAnalytics) List(_ context.Context, opt repository.PostAnalyticsListOptions) ([]repository.PostAnalyticsListItem, repository.PostAnalyticsOverview, error) {
	l.got = opt
	return []repository.PostAnalyticsListItem{{PostID: "p1"}}, repository.PostAnalyticsOverview{PostCount: 21}, nil
}

type namedPlatforms struct{ repository.PlatformRepository }

func (namedPlatforms) List(context.Context) ([]models.Platform, error) {
	return []models.Platform{{ID: "p-li", ZernioID: "linkedin", Name: "LinkedIn"}}, nil
}

// The posts list filters by the platform's name, caps the page, and
// refuses an unknown sort.
func TestListPostAnalytics(t *testing.T) {
	repo := &listedAnalytics{}
	d := Deps{Analytics: &analytics.Service{Repo: repo, Platforms: namedPlatforms{}}}
	ctx := analyticsTurn(t, nil)
	out, err := listPostAnalytics(ctx, d, ListPostAnalyticsInput{SortBy: "saves", Platform: "linkedin", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if repo.got.Platform != "LinkedIn" || repo.got.Limit != 25 || repo.got.SortBy != "saves" || repo.got.Order != "desc" || repo.got.Publisher != models.PublisherZernio {
		t.Fatalf("list = %+v", repo.got)
	}
	if p, ok := out.Data.(*analytics.PostsPage); !ok || p.Pages != 1 || p.Total != 21 || p.SortBy != "saves" || p.Order != "desc" || out.Applied.Platforms[0] != "linkedin" {
		t.Fatalf("out = %+v", out)
	}
	_, err = listPostAnalytics(ctx, d, ListPostAnalyticsInput{SortBy: "vibes"})
	refused(t, err)
}
