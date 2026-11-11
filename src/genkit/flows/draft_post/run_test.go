package draft_post

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// spreadDates distributes N posts across [start, end]: n==1 pins to start;
// otherwise the first lands on start and the last on end, evenly spaced.
func TestSpreadDates(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		n          int
		want       []string
	}{
		{"single pins to start", "2026-08-15", "2026-08-15", 1, []string{"2026-08-15"}},
		{"single ignores window end", "2026-08-15", "2026-08-29", 1, []string{"2026-08-15"}},
		{"three across two weeks", "2026-08-01", "2026-08-15", 3, []string{"2026-08-01", "2026-08-08", "2026-08-15"}},
		{"two endpoints", "2026-08-01", "2026-08-11", 2, []string{"2026-08-01", "2026-08-11"}},
		{"same-day multi collapses", "2026-08-20", "2026-08-20", 3, []string{"2026-08-20", "2026-08-20", "2026-08-20"}},
	}
	for _, c := range cases {
		got := spreadDates(day(c.start), day(c.end), c.n)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: spreadDates(%s, %s, %d) = %v, want %v", c.name, c.start, c.end, c.n, got, c.want)
		}
	}
}

// fakePlatformRepo is a minimal PlatformRepository returning a fixed list.
type fakePlatformRepo struct {
	repository.PlatformRepository
	platforms []models.Platform
}

func (f *fakePlatformRepo) List(context.Context) ([]models.Platform, error) {
	return f.platforms, nil
}

// resolvePlatform resolves the post-type slug: explicit wins, else the
// campaign's first selected type, else a platform slug; and it carries the
// platform's free-text constraints for the prompt.
func TestResolvePlatform(t *testing.T) {
	repo := &fakePlatformRepo{platforms: []models.Platform{
		{ID: "li", Name: "LinkedIn", PostTypes: models.PostTypeMap{"article": "Article"}, Constraints: "3000 chars max"},
	}}
	campaign := &models.Campaign{
		TargetPlatforms: models.CampaignPlatforms{
			{ID: "li", PostTypes: []string{"text-post", "article"}},
		},
	}

	// Explicit post type wins.
	got, err := resolvePlatform(t.Context(), campaign, "li", "article", repo)
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if got.Name != "LinkedIn" || got.PostType != "article" || got.Constraints != "3000 chars max" {
		t.Fatalf("explicit = %+v", got)
	}

	// No explicit type → the campaign's first selected slug for this platform.
	got, err = resolvePlatform(t.Context(), campaign, "li", "", repo)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if got.PostType != "text-post" {
		t.Fatalf("default post type = %q, want text-post", got.PostType)
	}

	// No explicit type and no campaign-selected slugs → the lexicographically
	// first platform slug (deterministic, not map-iteration order).
	multi := &fakePlatformRepo{platforms: []models.Platform{
		{ID: "x", Name: "X", PostTypes: models.PostTypeMap{"zeta": "Z", "alpha": "A", "mid": "M"}},
	}}
	got, err = resolvePlatform(t.Context(), &models.Campaign{}, "x", "", multi)
	if err != nil {
		t.Fatalf("map fallback: %v", err)
	}
	if got.PostType != "alpha" {
		t.Fatalf("map fallback post type = %q, want alpha (lexicographically first)", got.PostType)
	}

	// Unknown platform id → error.
	if _, err := resolvePlatform(t.Context(), campaign, "nope", "", repo); err == nil {
		t.Fatal("expected error for unknown platform id")
	}
}

// A draft is grounded by the assets it was written from. With none named,
// it is exploratory, unless its sources aren't tracked (chat research);
// then its grounding is left unset rather than claimed.
func TestDraftGrounding(t *testing.T) {
	mode := func(m *models.GroundingMode) string {
		if m == nil {
			return "unset"
		}
		return string(*m)
	}
	cases := []struct {
		req  DraftPostRequest
		want string
	}{
		{DraftPostRequest{UsedAssetIDs: []string{"a1"}}, "grounded"},
		{DraftPostRequest{}, "exploratory"},
		{DraftPostRequest{SourcesUnknown: true}, "unset"},
		{DraftPostRequest{SourcesUnknown: true, UsedAssetIDs: []string{"a1"}}, "grounded"},
	}
	for _, tc := range cases {
		if got := mode(draftGrounding(tc.req)); got != tc.want {
			t.Errorf("%+v = %s, want %s", tc.req, got, tc.want)
		}
	}
}

// A request with no window drafts undated posts that stay off the calendar;
// one window bound alone is refused.
func TestPersist_UndatedWithoutAWindow(t *testing.T) {
	if _, err := parseWindow("2026-12-01", ""); err == nil {
		t.Fatal("a lone window start was accepted")
	}
	for name, tc := range map[string]struct {
		start, end string
		dated      bool
	}{
		"undated": {"", "", false},
		"dated":   {"2026-12-01", "2026-12-01", true},
	} {
		w, err := parseWindow(tc.start, tc.end)
		if err != nil {
			t.Fatal(err)
		}
		posts := &savedPosts{}
		sink := &draftSink{
			campaign: &models.Campaign{ID: "c1", PublishingTime: "09:00"},
			window:   w,
			dates:    publishDates(w, 2),
			loc:      time.UTC,
			repos:    DraftPostRepos{Posts: posts},
		}
		dp, err := sink.persist(t.Context(), sink.dates[0], "T", "Body")
		if err != nil {
			t.Fatal(err)
		}
		if len(sink.dates) != 2 || (posts.rows[0].ScheduledAt != nil) != tc.dated || (dp.PublishDate != "") != tc.dated {
			t.Fatalf("%s: dates %v, scheduled %v, publish date %q", name, sink.dates, posts.rows[0].ScheduledAt, dp.PublishDate)
		}
	}
}
