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
	s := Build(campaign(), posts(many...), 2)
	if s.Goal == nil || s.Goal.Target != 10 || s.Goal.Planned != 40 || s.Goal.Room != 0 || s.Goal.Periods != 2 {
		t.Fatalf("goal = %+v", s.Goal)
	}

	s = Build(campaign(), posts(models.PostStatusDraft, models.PostStatusScheduled, models.PostStatusFailed, models.PostStatusNotPublished), 2)
	if s.Goal.Planned != 2 || s.Goal.Room != 8 {
		t.Fatalf("failed and not-published posts must not use up the goal: %+v", s.Goal)
	}
	want := []StatusCount{{models.PostStatusDraft, 1}, {models.PostStatusScheduled, 1}, {models.PostStatusFailed, 1}, {models.PostStatusNotPublished, 1}}
	if !slices.Equal(s.ByStatus, want) || s.Posts != 4 {
		t.Fatalf("by status = %+v, posts = %d", s.ByStatus, s.Posts)
	}
}

func TestBuild_NoGoal(t *testing.T) {
	c := campaign()
	c.EstimatedPostCount = nil
	if s := Build(c, nil, 0); s.Goal != nil {
		t.Fatalf("goal = %+v, want none", s.Goal)
	}
}

func TestBuild_PlatformsAndSources(t *testing.T) {
	s := Build(campaign(), nil, 3)
	if len(s.Platforms) != 2 || s.Platforms[0].Name != "LinkedIn" || !slices.Equal(s.Platforms[0].PostTypes, []string{"article"}) ||
		!slices.Equal(s.Platforms[1].PostTypes, []string{"text-post"}) {
		t.Fatalf("platforms = %+v", s.Platforms)
	}
	if s.Sources == nil || s.Sources.Ready != 3 {
		t.Fatalf("sources = %+v", s.Sources)
	}
	if s := Build(campaign(), nil, -1); s.Sources != nil {
		t.Fatalf("unknown sources = %+v, want nil", s.Sources)
	}
}
