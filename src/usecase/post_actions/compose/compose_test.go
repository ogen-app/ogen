package compose

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type oneCampaign struct {
	repository.CampaignRepository
	byID map[string]*models.Campaign
}

func (r oneCampaign) GetByID(_ context.Context, id string) (*models.Campaign, error) {
	if c, ok := r.byID[id]; ok {
		return c, nil
	}
	return nil, sql.ErrNoRows
}

func spring() *models.Campaign {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	return &models.Campaign{
		ID: "c1", StartDate: &start, EndDate: &end, PublishingTime: "09:00",
		TargetPlatforms: models.CampaignPlatforms{
			{ID: "linkedin", PostTypes: []string{"text-post", "carousel"}},
			{ID: "threads"},
		},
	}
}

func idea(id, campaign string, plan models.IdeaPlan) models.Idea {
	i := models.Idea{ID: id, Title: "Idea " + id, Plan: plan}
	if campaign != "" {
		i.CampaignID = &campaign
	}
	return i
}

func TestCampaignResolution(t *testing.T) {
	s := &Service{campaigns: oneCampaign{byID: map[string]*models.Campaign{"c1": spring(), "c2": {ID: "c2"}}}}
	cases := map[string]struct {
		asked string
		ideas []models.Idea
		want  string
		err   error
	}{
		"shared":           {"", []models.Idea{idea("a", "c1", models.IdeaPlan{}), idea("b", "", models.IdeaPlan{})}, "c1", nil},
		"asked wins":       {"c2", []models.Idea{idea("a", "c1", models.IdeaPlan{})}, "c2", nil},
		"none":             {"", []models.Idea{idea("a", "", models.IdeaPlan{})}, "", ErrNoCampaign},
		"mixed":            {"", []models.Idea{idea("a", "c1", models.IdeaPlan{}), idea("b", "c2", models.IdeaPlan{})}, "", ErrMixedCampaigns},
		"asked is unknown": {"c9", []models.Idea{idea("a", "c1", models.IdeaPlan{})}, "", ErrCampaignGone},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := s.campaign(t.Context(), tc.asked, tc.ideas)
			if !errors.Is(err, tc.err) || (err == nil && c.ID != tc.want) {
				t.Fatalf("campaign = %v, err = %v; want %s, %v", c, err, tc.want, tc.err)
			}
		})
	}
}

// buildOne builds the post for in's first target.
func buildOne(s *Service, in Input, ideas []models.Idea, c *models.Campaign) (*models.Post, error) {
	targets, err := compileTargets(in, ideas, c)
	if err != nil {
		return nil, err
	}
	return s.build(in, targets[0], ideas, c, nil)
}

// Each platform asked for gets its own post; the suggested post type goes
// only with its suggested platform, and a named type needs one platform.
func TestCompileTargets_SeveralPlatforms(t *testing.T) {
	planned := idea("a", "c1", models.IdeaPlan{PlatformID: new("linkedin"), PostType: new("carousel")})
	got, err := compileTargets(Input{PlatformIDs: []string{"threads", "linkedin", "threads"}}, []models.Idea{planned}, spring())
	if err != nil || !slices.Equal(got, []target{{"threads", ""}, {"linkedin", "carousel"}}) {
		t.Fatalf("targets = %+v, %v", got, err)
	}
	for name, tc := range map[string]struct {
		in   Input
		want error
	}{
		"type for two": {Input{PlatformIDs: []string{"threads", "linkedin"}, PostType: "carousel"}, ErrPostTypeOneFor},
		"one stranger": {Input{PlatformIDs: []string{"linkedin", "myspace"}}, ErrNotTarget},
	} {
		if _, err := compileTargets(tc.in, []models.Idea{planned}, spring()); !errors.Is(err, tc.want) || !IsValidation(err) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestBuild_PlanDefaultsAndOverrides(t *testing.T) {
	s := &Service{now: func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }}
	day, _ := models.ParseCalendarDate("2026-11-20")
	asset := "a1"
	first := idea("a", "c1", models.IdeaPlan{PlatformID: new("linkedin"), PostType: new("carousel")})
	first.SourceAssetID = &asset
	second := idea("b", "c1", models.IdeaPlan{PlatformID: new("threads"), PhaseID: new("ph2"), TargetDate: &day})
	second.SourceAssetID = &asset
	other := idea("c", "c9", models.IdeaPlan{PhaseID: new("elsewhere")})

	// The first idea plan that sets a field wins; a phase only from this
	// campaign's ideas.
	post, err := buildOne(s, Input{Actor: "u1"}, []models.Idea{other, first, second}, spring())
	if err != nil {
		t.Fatal(err)
	}
	if post.PlatformID != "linkedin" || post.PlatformPostType != "carousel" || *post.CampaignTypePhaseID != "ph2" ||
		post.Status != models.PostStatusPlanned || post.Title != "Idea c" || !slices.Equal(post.UsedAssetIDs, models.StringSlice{"a1"}) {
		t.Fatalf("post = %+v", post)
	}
	if post.ScheduledAt == nil || post.ScheduledAt.Format(time.DateOnly) != "2026-11-20" {
		t.Fatalf("scheduled_at = %v; want the target date at the publishing time", post.ScheduledAt)
	}

	// Naming another platform drops the first idea's post type for it.
	post, err = buildOne(s, Input{PlatformIDs: []string{"threads"}, Title: " Mine "}, []models.Idea{first}, spring())
	if err != nil || post.PlatformID != "threads" || post.PlatformPostType != "" || post.Title != "Mine" {
		t.Fatalf("override: %+v, %v", post, err)
	}

	// A stale suggestion is dropped; the same thing asked for is refused.
	stale := idea("s", "c1", models.IdeaPlan{PlatformID: new("myspace")})
	if post, err = buildOne(s, Input{}, []models.Idea{stale}, spring()); err != nil || post.PlatformID != "" {
		t.Fatalf("stale suggestion: %+v, %v", post, err)
	}
	// A stale suggested type goes on its own: the platform stays, named or not.
	staleType := idea("t", "c1", models.IdeaPlan{PlatformID: new("linkedin"), PostType: new("reel")})
	for _, in := range []Input{{}, {PlatformIDs: []string{"linkedin"}}} {
		if post, err = buildOne(s, in, []models.Idea{staleType}, spring()); err != nil || post.PlatformID != "linkedin" || post.PlatformPostType != "" {
			t.Fatalf("stale type, platforms %v: %+v, %v", in.PlatformIDs, post, err)
		}
	}
	// A suggested type without a platform is dropped.
	typeOnly := idea("o", "c1", models.IdeaPlan{PostType: new("carousel")})
	if post, err = buildOne(s, Input{}, []models.Idea{typeOnly}, spring()); err != nil || post.PlatformID != "" || post.PlatformPostType != "" {
		t.Fatalf("type-only suggestion: %+v, %v", post, err)
	}
	for name, tc := range map[string]struct {
		in   Input
		want error
	}{
		"not a target": {Input{PlatformIDs: []string{"myspace"}}, ErrNotTarget},
		"wrong type":   {Input{PlatformIDs: []string{"linkedin"}, PostType: "reel"}, ErrPostType},
		"type alone":   {Input{PostType: "reel"}, ErrPostTypeAlone},
	} {
		if _, err := buildOne(s, tc.in, []models.Idea{idea("x", "c1", models.IdeaPlan{})}, spring()); !errors.Is(err, tc.want) || !IsValidation(err) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestThesesNote(t *testing.T) {
	angle, blank := "Most buyers overpay\n- by a third", " \n "
	ideas := []models.Idea{
		{ID: "a", Title: "Pricing myths", Angle: &angle, Note: "ignored"},
		{ID: "b", Title: "Founder story", Angle: &blank, Note: "How it started"},
		{ID: "c", Title: "Bare"},
	}
	post := &models.Post{ID: "p1", CreatedAt: time.Now()}
	n, err := thesesNote(post, ideas, Input{Actor: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	want := "- **Pricing myths**\n  - Most buyers overpay\n  - by a third\n- **Founder story**\n  - How it started\n- **Bare**"
	if n.Body != want || n.Type != models.PostNoteTypeDraftThesis || n.Origin != models.PostNoteOriginManual || n.CreatedBy != "u1" {
		t.Fatalf("note = %+v\nbody:\n%s", n, n.Body)
	}
	if strings.Contains(n.Body, "ignored") {
		t.Fatal("an analysis idea's angle stands in for its note")
	}
	theses := Theses(ideas)
	if len(theses) != 3 || !slices.Equal(theses[0].Points, []string{"Most buyers overpay", "by a third"}) || theses[2].Title != "Bare" || theses[2].Points != nil {
		t.Fatalf("theses = %+v", theses)
	}
}

func TestExcerpt(t *testing.T) {
	if got := Excerpt("  Spring is back.\n\nBook   now. "); got != "Spring is back. Book now." {
		t.Fatalf("excerpt = %q", got)
	}
	long := Excerpt(strings.Repeat("word ", 100))
	if n := len([]rune(long)); n > excerptRunes || !strings.HasSuffix(long, "…") {
		t.Fatalf("long excerpt = %d runes: %q", n, long)
	}
}

func TestCompile_RefusesBeforeWriting(t *testing.T) {
	s := &Service{}
	many := make([]string, MaxIdeas+1)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	for name, ids := range map[string][]string{"none": nil, "blank": {" "}, "too many": many} {
		if _, err := s.Compile(t.Context(), Input{IdeaIDs: ids}); !errors.Is(err, ErrIdeaCount) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// dedupe stops one past the limit, however long the list.
func TestDedupe_StopsPastTheLimit(t *testing.T) {
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	if got := dedupe(ids, MaxIdeas); len(got) != MaxIdeas+1 {
		t.Fatalf("dedupe kept %d ids, want %d", len(got), MaxIdeas+1)
	}
	if got := dedupe([]string{" a", "b", "a ", "", "b"}, MaxIdeas); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("dedupe = %v", got)
	}
}

type memSeries struct {
	repository.SeriesRepository
	byID map[string]models.Series
}

func (r memSeries) GetByID(_ context.Context, id string) (*models.Series, error) {
	if s, ok := r.byID[id]; ok {
		return &s, nil
	}
	return nil, sql.ErrNoRows
}

func digestSeries() memSeries {
	format := models.ContentFormatDigest
	return memSeries{byID: map[string]models.Series{"s1": {ID: "s1", Name: "Tuesday digest", ContentFormat: &format}}}
}

// A post made from a series idea takes the series and its format, and its
// plan date keeps the series' weekday even on a day the campaign doesn't
// publish; a date the call names still snaps.
func TestBuild_SeriesIdea(t *testing.T) {
	s := (&Service{now: time.Now}).WithSeries(digestSeries())
	tuesday, _ := models.ParseCalendarDate("2026-11-17")
	c := spring()
	c.PublishingDays = []string{"mon", "wed", "fri"}
	plain := idea("p", "c1", models.IdeaPlan{PlatformID: new("linkedin")})
	reserved := idea("r", "c1", models.IdeaPlan{PlatformID: new("linkedin"), TargetDate: &tuesday})
	reserved.SeriesID = new("s1")
	ideas := []models.Idea{plain, reserved}

	series, err := s.seriesOf(t.Context(), ideas)
	if err != nil || series == nil || series.id != "s1" || *series.format != models.ContentFormatDigest {
		t.Fatalf("series = %+v, %v", series, err)
	}
	targets, _ := compileTargets(Input{}, ideas, c)
	post, err := s.build(Input{}, targets[0], ideas, c, series)
	if err != nil {
		t.Fatal(err)
	}
	if post.SeriesID == nil || *post.SeriesID != "s1" || post.ContentFormat == nil || *post.ContentFormat != models.ContentFormatDigest {
		t.Fatalf("post series = %v, format = %v", post.SeriesID, post.ContentFormat)
	}
	if got := post.ScheduledAt.Format(time.DateOnly); got != "2026-11-17" {
		t.Fatalf("scheduled on %s; want the series' Tuesday", got)
	}

	post, err = s.build(Input{TargetDate: &tuesday}, targets[0], ideas, c, series)
	if err != nil {
		t.Fatal(err)
	}
	if got := post.ScheduledAt.Format(time.DateOnly); got != "2026-11-18" {
		t.Fatalf("asked date scheduled on %s; want it snapped to Wednesday", got)
	}

	// A plain idea's date ahead of the series idea is the plan's date, and it
	// snaps: only a date the series idea set keeps its weekday.
	sunday, _ := models.ParseCalendarDate("2026-11-15")
	dated := idea("d", "c1", models.IdeaPlan{PlatformID: new("linkedin"), TargetDate: &sunday})
	mixed := []models.Idea{dated, reserved}
	post, err = s.build(Input{}, targets[0], mixed, c, series)
	if err != nil {
		t.Fatal(err)
	}
	if got := post.ScheduledAt.Format(time.DateOnly); got != "2026-11-16" || post.SeriesID == nil {
		t.Fatalf("plain idea's Sunday scheduled on %s (series %v); want it snapped to Monday", got, post.SeriesID)
	}

	// A deleted series, or no series idea, stamps nothing.
	gone := reserved
	gone.SeriesID = new("deleted")
	for name, ideas := range map[string][]models.Idea{"deleted": {gone}, "none": {plain}} {
		if series, err := s.seriesOf(t.Context(), ideas); series != nil || err != nil {
			t.Errorf("%s: series = %+v, %v", name, series, err)
		}
	}
}

// sqidCampaign targets its platforms by id, as a stored campaign does, with
// the platform rows a read hydrates.
func sqidCampaign() *models.Campaign {
	c := spring()
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "Xk2q", PostTypes: []string{"carousel"}}, {ID: "Pd8w"}, {ID: "Tw9z"}}
	c.Platforms = []models.Platform{{ID: "Xk2q", Name: "LinkedIn", ZernioID: "linkedin"}, {ID: "Pd8w", Name: "Threads", ZernioID: "threads"}, {ID: "Tw9z", Name: "X (Twitter)", ZernioID: "twitter"}}
	return c
}

// A platform asked for by its id, slug or name, in any case, is the
// campaign's platform id; a stranger is refused naming the campaign's.
func TestCompileTargets_PlatformBySlugOrName(t *testing.T) {
	got, err := compileTargets(Input{PlatformIDs: []string{"threads", " LinkedIn ", "Pd8w", "THREADS", "x"}}, nil, sqidCampaign())
	if err != nil || !slices.Equal(got, []target{{"Pd8w", ""}, {"Xk2q", ""}, {"Tw9z", ""}}) {
		t.Fatalf("targets = %+v, %v", got, err)
	}
	// A blank platform is dropped, not matched to one without a slug.
	noSlug := sqidCampaign()
	noSlug.Platforms[1].ZernioID = ""
	got, err = compileTargets(Input{PlatformIDs: []string{"Xk2q", " "}}, nil, noSlug)
	if err != nil || !slices.Equal(got, []target{{"Xk2q", ""}}) {
		t.Fatalf("blank: targets = %+v, %v", got, err)
	}
	_, err = compileTargets(Input{PlatformIDs: []string{"myspace"}}, nil, sqidCampaign())
	if !errors.Is(err, ErrNotTarget) || !IsValidation(err) ||
		!strings.Contains(err.Error(), `"myspace"`) || !strings.Contains(err.Error(), "LinkedIn (linkedin), Threads (threads), X (Twitter) (twitter)") {
		t.Fatalf("err = %v; want ErrNotTarget naming the campaign's platforms", err)
	}
}
