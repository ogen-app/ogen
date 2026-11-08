package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type analyticsRepo struct {
	repository.PostAnalyticsRepository
	current map[string]*models.PostAnalytics
	snaps   []models.PostAnalyticsSnapshot
	samples []repository.ReachAgeSample
	life    []repository.LifespanSample
}

func (r analyticsRepo) CurrentMetricsByPostID(context.Context) (map[string]*models.PostAnalytics, error) {
	return r.current, nil
}

func (r analyticsRepo) GetByPostID(_ context.Context, id string) (*models.PostAnalytics, error) {
	return r.current[id], nil
}

func (r analyticsRepo) SnapshotsByPostID(context.Context, string) ([]models.PostAnalyticsSnapshot, error) {
	return r.snaps, nil
}

func (r analyticsRepo) ReachByAgeSamples(context.Context) ([]repository.ReachAgeSample, error) {
	return r.samples, nil
}

func (r analyticsRepo) LifespanSamples(context.Context, time.Time) ([]repository.LifespanSample, error) {
	return r.life, nil
}

// settled is n posts, ids prefix0.., that have run their course: a lifespan
// curve needs eight.
func settled(prefix string, n int) []repository.LifespanSample {
	var out []repository.LifespanSample
	for i := range n {
		id := fmt.Sprintf("%s%d", prefix, i)
		out = append(out,
			repository.LifespanSample{PostID: id, AgeHours: 0, Reach: 10},
			repository.LifespanSample{PostID: id, AgeHours: 24, Reach: 60},
			repository.LifespanSample{PostID: id, AgeHours: 120, Reach: 100})
	}
	return out
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
	for _, in := range []string{"0d", "3y", "d", "-1w", "9223372036854775807mo", "9223372036854775807w", "99999999999999999999d"} {
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
		{ID: "fresh", PublisherPostID: "z2", Title: "Fresh", Status: models.PostStatusPublished, Platform: &models.Platform{Name: "Instagram"},
			PublishedAt: published, PublishedURL: "https://ig.example/2"},
		{ID: "queued", PublisherPostID: "z3", Title: "Queued", Status: models.PostStatusScheduled},
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
	// Scheduled: a publisher id but not out, so not awaiting the platform.
	if _, _, err := s.PostStats(ctx, "queued"); !errors.Is(err, ErrNotPublishedViaPublisher) {
		t.Fatalf("scheduled post: %v", err)
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

	// Not refreshed yet: the post's own platform and permalink, awaiting the
	// platform.
	resp, _, err = s.PostStats(ctx, "fresh")
	if err != nil || resp.Post.Platform != "Instagram" || resp.Post.OpenURL != "https://ig.example/2" || resp.Overview.State != "awaiting_platform" {
		t.Fatalf("fresh = %+v, %v", resp, err)
	}

	if resp, reason, err := (&Service{}).PostStats(ctx, "live"); resp != nil || reason != ReasonNotConfigured || err != nil {
		t.Fatalf("no analytics db = %v, %q, %v", resp, reason, err)
	}
}

// A platform's lessons draw their lifespan from that platform's posts alone:
// ten settled posts make a workspace curve, the six on LinkedIn do not.
func TestLessons_NarrowsLifespan(t *testing.T) {
	pub := new(day("2026-09-01"))
	repo := analyticsRepo{current: map[string]*models.PostAnalytics{}}
	var posts []models.Post
	for i := range 10 {
		id, platformID := fmt.Sprintf("p%d", i), "p-li"
		if i >= 6 {
			platformID = "p-x"
		}
		repo.current[id] = &models.PostAnalytics{PostID: id, Platform: "LinkedIn", PublishedAt: pub}
		posts = append(posts, models.Post{ID: id, PlatformID: platformID})
	}
	repo.life = settled("p", 10)
	s := &Service{Repo: repo, Posts: readPosts{posts: posts}, Platforms: platforms{}}
	ctx := t.Context()

	all, _, err := s.Lessons(ctx, nil, LessonsOptions{})
	if err != nil || all.Lifespan == nil || all.Lifespan.InsufficientHistory || all.Lifespan.SettledPosts != 10 {
		t.Fatalf("workspace lifespan = %+v, %v", all.Lifespan, err)
	}
	sc, err := s.Scope(ctx, []string{"linkedin"}, "")
	if err != nil {
		t.Fatal(err)
	}
	li, _, err := s.Lessons(ctx, sc, LessonsOptions{})
	if err != nil || li.Lifespan == nil || !li.Lifespan.InsufficientHistory {
		t.Fatalf("linkedin lifespan = %+v, %v; want insufficient_history", li.Lifespan, err)
	}
}

// The post's snapshots, the workspace's reach at age and its lifespan each
// reach the response: the series, the bands and the narrative.
func TestPostStats_History(t *testing.T) {
	published := day("2026-10-20")
	repo := analyticsRepo{
		current: map[string]*models.PostAnalytics{
			"live": {PostID: "live", Platform: "LinkedIn", PublishedAt: &published, Reach: 500, LastCheckedAt: day("2026-11-01")},
		},
		snaps: []models.PostAnalyticsSnapshot{
			{OccurredAt: published.Add(6 * time.Hour), Reach: 200},
			{OccurredAt: published.Add(30 * time.Hour), Reach: 500},
		},
		life: settled("s", 8),
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		repo.samples = append(repo.samples,
			repository.ReachAgeSample{Platform: "LinkedIn", PostID: id, AgeDay: 0, Reach: 50},
			repository.ReachAgeSample{Platform: "LinkedIn", PostID: id, AgeDay: 1, Reach: 100})
	}
	s := &Service{Repo: repo, Posts: readPosts{posts: []models.Post{{ID: "live", PublisherPostID: "z1"}}}}

	resp, _, err := s.PostStats(t.Context(), "live")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Series["reach"]; len(got) != 3 || got[1].AgeHours != 6 || got[2].Value != 500 {
		t.Fatalf("reach series = %+v; want the snapshots after the origin", got)
	}
	reach := resp.Overview.Cards[0]
	if reach.Metric != "reach" || reach.AgainstTypical == nil || *reach.AgainstTypical != 5 || reach.Band != "above" {
		t.Fatalf("reach card = %+v; want 5x the typical 100", reach)
	}
	var ids []string
	for _, n := range resp.Overview.Narrative {
		ids = append(ids, n.ID)
	}
	if !slices.Equal(ids, []string{"pace", "half_life"}) || resp.StillCounting {
		t.Fatalf("narrative = %v, still counting %v; want the lifespan's", ids, resp.StillCounting)
	}
}
