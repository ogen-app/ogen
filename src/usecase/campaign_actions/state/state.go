// Package state builds the snapshot of a campaign the assistant reads before
// acting: the goal and how much room is left under it, overall and in the
// current goal period, the posts by status, what each platform may publish,
// the calendar's problems, and the sources there are to ground generation
// in. It is pure: callers load the rows and run the calendar check.
package state

import (
	"maps"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// Snapshot is a campaign's state at the start of an assistant turn. Its JSON
// is what the client cites under a reply.
type Snapshot struct {
	// Goal is nil when the campaign has no post goal.
	Goal     *Goal         `json:"goal,omitempty"`
	Posts    int           `json:"posts"`
	ByStatus []StatusCount `json:"byStatus"`
	// PostsUnknown is set when the posts could not be read, so Posts and
	// ByStatus are empty without meaning the campaign has none.
	PostsUnknown bool       `json:"postsUnknown,omitzero"`
	Platforms    []Platform `json:"platforms"`
	// Sources is nil when the attached assets could not be counted.
	Sources *Sources `json:"sources,omitempty"`
	// Calendar is nil when the calendar could not be checked.
	Calendar *Calendar `json:"calendar,omitempty"`
}

// Calendar summarises the calendar check's findings: how many, of what kind,
// and the first few as the check orders them, warnings first.
type Calendar struct {
	Total int         `json:"total"`
	Kinds []KindCount `json:"kinds"`
	Top   []string    `json:"top"`
}

// KindCount is how many findings of one kind there are.
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Period is the goal period today falls in, against its share of the goal.
type Period struct {
	Label   string `json:"label"`
	Target  int    `json:"target"`
	Planned int    `json:"planned"`
	// Left is how many more posts fit in the period; 0 once it is met.
	Left int `json:"left"`
}

// Goal is the campaign's post goal against the posts it already has.
type Goal struct {
	Cadence   string `json:"cadence"`
	PerPeriod int    `json:"perPeriod"`
	Periods   int    `json:"periods"`
	// Target is the posts the whole campaign aims for.
	Target int `json:"target"`
	// Planned counts the posts already in the campaign that could still
	// publish: everything but failed and not-published.
	Planned int `json:"planned"`
	// Room is how many more posts fit under the target; 0 once it is met.
	Room int `json:"room"`
	// Unknown is set when the posts could not be read: Planned and Room
	// are then 0 and say nothing about the room left.
	Unknown bool `json:"unknown,omitzero"`
	// Period is the current goal period; nil outside the campaign's dates,
	// or when the posts could not be read.
	Period *Period `json:"period,omitempty"`
}

// StatusCount is how many posts are in one status.
type StatusCount struct {
	Status models.PostStatus `json:"status"`
	Count  int               `json:"count"`
}

// Platform is a target platform and the post types the campaign may use on it.
type Platform struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	PostTypes []string `json:"postTypes"`
}

// Sources says what material generation has to draw on: the campaign's
// attached assets, or the whole bank when it attached none; how many of them
// are ready to use, and how many are analysed.
type Sources struct {
	// WholeBank is set when the campaign attached no assets, so it draws on
	// every asset in the workspace.
	WholeBank bool `json:"wholeBank"`
	Attached  int  `json:"attached"`
	Ready     int  `json:"ready"`
	// Analysed is -1 when the analyses could not be read.
	Analysed int `json:"analysed"`
	// Items are the campaign's attached sources one by one, up to
	// MaxSourceItems, with how far each got; empty when it draws on the
	// whole bank.
	Items []SourceItem `json:"items,omitempty"`
}

// MaxSourceItems caps the sources a snapshot lists one by one.
const MaxSourceItems = 20

// The stages a source can be at, as SourceItem.Status names them.
const (
	SourceReady      = "ready"
	SourceAnalysing  = "analysing"
	SourceUnreadable = "unreadable"
)

// SourceItem is one attached source and how far it got: ready to write
// from, still being read or analysed, or unreadable.
type SourceItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status"`
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

// Build summarises a campaign from its posts (only Status is read) and its
// sources, nil when unknown.
func Build(c *models.Campaign, posts []models.Post, sources *Sources) Snapshot {
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
	s.Sources = sources
	return s
}

// WithPeriod sets the goal's current period from its share of the goal and
// the posts dated in it.
func (s Snapshot) WithPeriod(label string, target, planned int) Snapshot {
	if s.Goal == nil || s.Goal.Unknown {
		return s
	}
	g := *s.Goal
	g.Period = &Period{Label: label, Target: target, Planned: planned, Left: max(target-planned, 0)}
	s.Goal = &g
	return s
}

// WithoutPosts marks the post counts unknown, for when the posts could not
// be read. The goal stays, with its room unknown rather than the whole target.
func (s Snapshot) WithoutPosts() Snapshot {
	s.Posts, s.ByStatus, s.PostsUnknown = 0, nil, true
	if s.Goal != nil {
		g := *s.Goal
		g.Planned, g.Room, g.Unknown = 0, 0, true
		s.Goal = &g
	}
	return s
}

// buildGoal measures planned posts against the campaign's goal; nil when
// the campaign has no goal.
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

// platformName returns the platform's display name, or its id when the
// campaign does not carry it.
func platformName(c *models.Campaign, id string) string {
	for _, p := range c.Platforms {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}
