package analytics

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type analyticsRepo struct {
	repository.PostAnalyticsRepository
	current map[string]*models.PostAnalytics
}

func (r analyticsRepo) CurrentMetricsByPostID(context.Context) (map[string]*models.PostAnalytics, error) {
	return r.current, nil
}

func (r analyticsRepo) GetByPostID(_ context.Context, id string) (*models.PostAnalytics, error) {
	return r.current[id], nil
}

func (analyticsRepo) SnapshotsByPostID(context.Context, string) ([]models.PostAnalyticsSnapshot, error) {
	return nil, nil
}

func (analyticsRepo) ReachByAgeSamples(context.Context) ([]repository.ReachAgeSample, error) {
	return nil, nil
}

func (analyticsRepo) LifespanSamples(context.Context, time.Time) ([]repository.LifespanSample, error) {
	return nil, nil
}

type readPosts struct {
	repository.PostRepository
	posts []models.Post
}

func (r readPosts) ListPublishedSince(context.Context, time.Time) ([]models.Post, error) {
	return slices.Clone(r.posts), nil
}

func (r readPosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	for i := range r.posts {
		if r.posts[i].ID == id {
			return &r.posts[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

func day(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

func TestLessons_NarrowsBeforeBuilding(t *testing.T) {
	pub := func(s string) *time.Time { return new(day(s)) }
	repo := analyticsRepo{current: map[string]*models.PostAnalytics{
		"a": {PostID: "a", Platform: "LinkedIn", PublishedAt: pub("2026-10-01"), LastCheckedAt: day("2026-11-02")},
		"b": {PostID: "b", Platform: "X", PublishedAt: pub("2026-10-02"), LastCheckedAt: day("2026-11-03")},
		"c": {PostID: "c", Platform: "LinkedIn", PublishedAt: pub("2026-08-01")},
	}}
	posts := readPosts{posts: []models.Post{{ID: "a", PlatformID: "p-li"}, {ID: "b", PlatformID: "p-x"}, {ID: "c", PlatformID: "p-li"}}}
	var gotSince time.Time
	s := &Service{Repo: repo, Posts: posts, Platforms: platforms{},
		LifespanSamples: func(_ context.Context, since time.Time) ([]repository.LifespanSample, error) {
			gotSince = since
			return []repository.LifespanSample{{PostID: "a"}, {PostID: "b"}}, nil
		}}
	ctx := t.Context()

	sc, err := s.Scope(ctx, []string{"linkedin"}, "")
	if err != nil {
		t.Fatal(err)
	}
	since := day("2026-09-01")
	resp, _, err := s.Lessons(ctx, sc, LessonsOptions{Since: &since, Metric: "saves"})
	if err != nil || resp == nil {
		t.Fatalf("lessons = %v, %v", resp, err)
	}
	// Only a: b is another platform's, c predates since.
	if resp.Scope.MeasuredPosts != 1 || resp.Scope.Metric != "saves" || resp.Scope.TrendWindowDays != DefaultTrendWindowDays {
		t.Fatalf("scope = %+v", resp.Scope)
	}
	if !gotSince.Equal(since) || !resp.UpdatedAt.Equal(day("2026-11-03")) {
		t.Fatalf("since = %v, updated = %v", gotSince, resp.UpdatedAt)
	}

	all, _, err := s.Lessons(ctx, nil, LessonsOptions{})
	if err != nil || all.Scope.MeasuredPosts != 3 || all.Scope.Since != nil || all.Scope.Metric != "reach" {
		t.Fatalf("workspace lessons = %+v, %v", all, err)
	}

	empty := &Service{Repo: analyticsRepo{}}
	if resp, reason, err := empty.Lessons(ctx, nil, LessonsOptions{}); resp != nil || reason != ReasonNoData || err != nil {
		t.Fatalf("no data = %v, %q, %v", resp, reason, err)
	}
}

func TestParseTrendWindow(t *testing.T) {
	for in, want := range map[string]int{"": 90, "30d": 30, "12w": 84, "3mo": 90} {
		if got, ok := ParseTrendWindow(in); !ok || got != want {
			t.Errorf("ParseTrendWindow(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"0d", "3y", "d", "-1w"} {
		if _, ok := ParseTrendWindow(in); ok {
			t.Errorf("ParseTrendWindow(%q) accepted", in)
		}
	}
}

func TestPostStats(t *testing.T) {
	published := new(day("2026-10-20"))
	repo := analyticsRepo{current: map[string]*models.PostAnalytics{
		"live": {
			PostID: "live", Platform: "LinkedIn", Title: "Refreshed title", PublishedAt: published, Reach: 120,
			LastCheckedAt: day("2026-11-01"),
			PlatformAnalytics: models.PlatformAnalyticsList{
				{Platform: "X", PlatformPostURL: "https://x.example/1"},
				{Platform: "linkedin", PlatformPostURL: "https://li.example/1", AccountUsername: "acme"},
			},
		},
	}}
	posts := readPosts{posts: []models.Post{
		{ID: "live", PublisherPostID: "z1", Title: "Draft title", MediaURLs: models.StringSlice{"a", "b"}},
		{ID: "fresh", PublisherPostID: "z2", Title: "Fresh", Platform: &models.Platform{Name: "Instagram"}, PublishedAt: published},
		{ID: "local", Title: "Never sent"},
	}}
	s := &Service{Repo: repo, Posts: posts}
	ctx := t.Context()

	if _, _, err := s.PostStats(ctx, "nope"); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("unknown post: %v", err)
	}
	if _, _, err := s.PostStats(ctx, "local"); !errors.Is(err, ErrNotPublishedViaPublisher) {
		t.Fatalf("unpublished post: %v", err)
	}

	resp, _, err := s.PostStats(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	h := resp.Post
	if h.Title != "Refreshed title" || h.Platform != "LinkedIn" || h.OpenURL != "https://li.example/1" ||
		h.MediaFormat != "carousel" || !h.PublishedAt.Equal(*published) || h.Account.DisplayName != "acme" {
		t.Fatalf("header = %+v", h)
	}
	if resp.Overview.State != "ready" || !resp.UpdatedAt.Equal(day("2026-11-01")) {
		t.Fatalf("overview = %+v, updated = %v", resp.Overview, resp.UpdatedAt)
	}

	// Not refreshed yet: the post's own platform, awaiting the platform.
	resp, _, err = s.PostStats(ctx, "fresh")
	if err != nil || resp.Post.Platform != "Instagram" || resp.Overview.State != "awaiting_platform" {
		t.Fatalf("fresh = %+v, %v", resp, err)
	}

	if resp, reason, err := (&Service{}).PostStats(ctx, "live"); resp != nil || reason != ReasonNotConfigured || err != nil {
		t.Fatalf("no analytics db = %v, %q, %v", resp, reason, err)
	}
}
