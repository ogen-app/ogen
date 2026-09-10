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

// The streaming scanner must yield each top-level object exactly once, tolerate
// split chunks, and not be fooled by braces or brackets inside strings.
func TestJSONObjScanner(t *testing.T) {
	s := newJSONObjScanner()
	var got []string
	// Feed the array in awkward chunk boundaries.
	feed := []string{
		`[{"title":"a","content":"hello `,
		`{world}"},`,
		`{"title":"b",`,
		`"content":"line1\nline2"}]`,
	}
	for _, f := range feed {
		got = append(got, s.push(f)...)
	}
	want := []string{
		`{"title":"a","content":"hello {world}"}`,
		`{"title":"b","content":"line1\nline2"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanner objects = %v, want %v", got, want)
	}
}

func TestStripFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[{\"a\":1}]", `[{"a":1}]`},
		{"```json\n[{\"a\":1}]\n```", `[{"a":1}]`},
		{"```\n[1,2]\n```", `[1,2]`},
		{"  [1]  ", `[1]`},
	}
	for _, c := range cases {
		if got := stripFences(c.in); got != c.want {
			t.Errorf("stripFences(%q) = %q, want %q", c.in, got, c.want)
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
