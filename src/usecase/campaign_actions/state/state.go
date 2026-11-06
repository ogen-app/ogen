// Package state builds the snapshot of a campaign the assistant reads before
// acting: the goal and how much room is left under it, the posts by status,
// what each platform may publish, and whether there are sources to ground
// generation in. It is pure: callers load the rows.
package state

import (
	"maps"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// Snapshot is a campaign's state at the start of an assistant turn.
type Snapshot struct {
	// Goal is nil when the campaign has no post goal.
	Goal      *Goal
	Posts     int
	ByStatus  []StatusCount
	Platforms []Platform
	// Sources is nil when the attached assets could not be counted.
	Sources *Sources
}

// Goal is the campaign's post goal against the posts it already has.
type Goal struct {
	Cadence   string
	PerPeriod int
	Periods   int
	// Target is the posts the whole campaign aims for.
	Target int
	// Planned counts the posts already in the campaign that could still
	// publish: everything but failed and not-published.
	Planned int
	// Room is how many more posts fit under the target; 0 once it is met.
	Room int
}

// StatusCount is how many posts are in one status.
type StatusCount struct {
	Status models.PostStatus
	Count  int
}

// Platform is a target platform and the post types the campaign may use on it.
type Platform struct {
	ID        string
	Name      string
	PostTypes []string
}

// Sources says whether generation has the campaign's own material to draw on.
type Sources struct {
	Ready int
}

var statusOrder = []models.PostStatus{
	models.PostStatusDraft,
	models.PostStatusReadyForPublish,
	models.PostStatusScheduled,
	models.PostStatusScheduledForManualPublish,
	models.PostStatusPublished,
	models.PostStatusFailed,
	models.PostStatusNotPublished,
}

// Build summarises a campaign from its posts (only Status is read) and the
// number of its ready attached assets; readyAssets < 0 means unknown.
func Build(c *models.Campaign, posts []models.Post, readyAssets int) Snapshot {
	counts := make(map[models.PostStatus]int, len(statusOrder))
	for _, p := range posts {
		counts[p.Status]++
	}
	s := Snapshot{Posts: len(posts)}
	for _, st := range statusOrder {
		if n := counts[st]; n > 0 {
			s.ByStatus = append(s.ByStatus, StatusCount{Status: st, Count: n})
		}
	}
	s.Goal = buildGoal(c, len(posts)-counts[models.PostStatusFailed]-counts[models.PostStatusNotPublished])
	for _, tp := range c.TargetPlatforms {
		s.Platforms = append(s.Platforms, Platform{ID: tp.ID, Name: platformName(c, tp.ID), PostTypes: AllowedPostTypes(c, tp.ID)})
	}
	if readyAssets >= 0 {
		s.Sources = &Sources{Ready: readyAssets}
	}
	return s
}

func buildGoal(c *models.Campaign, planned int) *Goal {
	if c.EstimatedPostCount == nil || *c.EstimatedPostCount <= 0 {
		return nil
	}
	loc, err := settings.ResolveTimezone(c.Timezone)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	target := campaigngoal.EffectiveCount(c.EstimatedPostCount, c.GoalCadence, c.StartDate, c.EndDate, loc)
	return &Goal{
		Cadence:   c.GoalCadence,
		PerPeriod: *c.EstimatedPostCount,
		Periods:   campaigngoal.Periods(c.GoalCadence, c.StartDate, c.EndDate, loc),
		Target:    target,
		Planned:   planned,
		Room:      max(target-planned, 0),
	}
}

// AllowedPostTypes returns the post-type slugs the campaign may publish on a
// platform: the campaign's own selection when it made one, otherwise every
// type the platform supports. It mirrors content_plan's platform resolution,
// so a type that passes here also passes generation.
func AllowedPostTypes(c *models.Campaign, platformID string) []string {
	for _, tp := range c.TargetPlatforms {
		if tp.ID == platformID && len(tp.PostTypes) > 0 {
			return tp.PostTypes
		}
	}
	for _, p := range c.Platforms {
		if p.ID == platformID {
			return slices.Sorted(maps.Keys(p.PostTypes))
		}
	}
	return nil
}

func platformName(c *models.Campaign, id string) string {
	for _, p := range c.Platforms {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}
