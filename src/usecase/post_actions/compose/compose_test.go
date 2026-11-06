package compose

import (
	"context"
	"database/sql"
	"errors"
	"slices"
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
	post, err := s.build(Input{Actor: "u1"}, []models.Idea{other, first, second}, spring())
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
	post, err = s.build(Input{PlatformID: "threads", Title: " Mine "}, []models.Idea{first}, spring())
	if err != nil || post.PlatformID != "threads" || post.PlatformPostType != "" || post.Title != "Mine" {
		t.Fatalf("override: %+v, %v", post, err)
	}

	// A stale suggestion is dropped; the same thing asked for is refused.
	stale := idea("s", "c1", models.IdeaPlan{PlatformID: new("myspace")})
	if post, err = s.build(Input{}, []models.Idea{stale}, spring()); err != nil || post.PlatformID != "" {
		t.Fatalf("stale suggestion: %+v, %v", post, err)
	}
	for name, tc := range map[string]struct {
		in   Input
		want error
	}{
		"not a target": {Input{PlatformID: "myspace"}, ErrNotTarget},
		"wrong type":   {Input{PlatformID: "linkedin", PostType: "reel"}, ErrPostType},
		"type alone":   {Input{PostType: "reel"}, ErrPostTypeAlone},
	} {
		if _, err := s.build(tc.in, []models.Idea{idea("x", "c1", models.IdeaPlan{})}, spring()); !errors.Is(err, tc.want) || !IsValidation(err) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestThesesNote(t *testing.T) {
	angle := "Most buyers overpay\n- by a third"
	ideas := []models.Idea{
		{ID: "a", Title: "Pricing myths", Angle: &angle, Note: "ignored"},
		{ID: "b", Title: "Founder story", Note: "How it started"},
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
