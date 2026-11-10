package state

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func day(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

func posts(statuses ...models.PostStatus) []models.Post {
	out := make([]models.Post, len(statuses))
	for i, s := range statuses {
		out[i] = models.Post{Status: s}
	}
	return out
}

func campaign() *models.Campaign {
	perPeriod := 5
	return &models.Campaign{
		EstimatedPostCount: &perPeriod, GoalCadence: "month",
		StartDate: day("2026-04-01"), EndDate: day("2026-05-31"),
		Platforms: []models.Platform{
			{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"article": "Article", "text-post": "Text"}},
			{ID: "pl2", Name: "Threads", PostTypes: models.PostTypeMap{"text-post": "Text"}},
		},
		TargetPlatforms: models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}, {ID: "pl2"}},
	}
}

// The Daria case: the goal is 10 and 40 posts exist, so there is no room.
func TestBuild_GoalRoom(t *testing.T) {
	var many []models.PostStatus
	for range 40 {
		many = append(many, models.PostStatusDraft)
	}
	s := Build(campaign(), posts(many...), nil)
	if s.Goal == nil || s.Goal.Target != 10 || s.Goal.Planned != 40 || s.Goal.Room != 0 || s.Goal.Periods != 2 {
		t.Fatalf("goal = %+v", s.Goal)
	}

	s = Build(campaign(), posts(models.PostStatusDraft, models.PostStatusScheduled, models.PostStatusFailed, models.PostStatusNotPublished), nil)
	if s.Goal.Planned != 2 || s.Goal.Room != 8 {
		t.Fatalf("failed and not-published posts must not use up the goal: %+v", s.Goal)
	}
	want := []StatusCount{{models.PostStatusDraft, 1}, {models.PostStatusScheduled, 1}, {models.PostStatusFailed, 1}, {models.PostStatusNotPublished, 1}}
	if !slices.Equal(s.ByStatus, want) || s.Posts != 4 {
		t.Fatalf("by status = %+v, posts = %d", s.ByStatus, s.Posts)
	}
}

// Posts that could not be read leave the room unknown, not the whole target.
func TestSnapshot_WithoutPosts(t *testing.T) {
	s := Build(campaign(), nil, nil).WithoutPosts()
	if s.Goal == nil || !s.Goal.Unknown || s.Goal.Room != 0 || s.Goal.Target != 10 || !s.PostsUnknown {
		t.Fatalf("goal = %+v, posts unknown = %v", s.Goal, s.PostsUnknown)
	}
	c := campaign()
	c.EstimatedPostCount = nil
	if s := Build(c, nil, nil).WithoutPosts(); s.Goal != nil || !s.PostsUnknown {
		t.Fatalf("no goal: goal = %+v, posts unknown = %v", s.Goal, s.PostsUnknown)
	}
}

func TestBuild_NoGoal(t *testing.T) {
	c := campaign()
	c.EstimatedPostCount = nil
	if s := Build(c, nil, nil); s.Goal != nil {
		t.Fatalf("goal = %+v, want none", s.Goal)
	}
}

func TestBuild_PlatformsAndSources(t *testing.T) {
	s := Build(campaign(), nil, &Sources{Attached: 4, Ready: 3, Analysed: 2})
	if len(s.Platforms) != 2 || s.Platforms[0].Name != "LinkedIn" || !slices.Equal(s.Platforms[0].PostTypes, []string{"article"}) ||
		!slices.Equal(s.Platforms[1].PostTypes, []string{"text-post"}) {
		t.Fatalf("platforms = %+v", s.Platforms)
	}
	if s.Sources == nil || s.Sources.Ready != 3 {
		t.Fatalf("sources = %+v", s.Sources)
	}
	if s := Build(campaign(), nil, nil); s.Sources != nil {
		t.Fatalf("unknown sources = %+v, want nil", s.Sources)
	}
}

// The current period's slots: its share of the goal less the posts dated in
// it; none once the posts are unknown.
func TestSnapshot_WithPeriod(t *testing.T) {
	s := Build(campaign(), posts(models.PostStatusDraft), nil).WithPeriod("This week", 3, 4)
	if p := s.Goal.Period; p == nil || p.Label != "This week" || p.Left != 0 {
		t.Fatalf("period = %+v", s.Goal.Period)
	}
	if p := Build(campaign(), nil, nil).WithPeriod("This week", 3, 1).Goal.Period; p.Left != 2 {
		t.Fatalf("period = %+v", p)
	}
	if s := Build(campaign(), nil, nil).WithoutPosts().WithPeriod("This week", 3, 1); s.Goal.Period != nil {
		t.Fatalf("unknown posts got a period: %+v", s.Goal.Period)
	}
}
