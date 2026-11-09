// Package calendarcheck finds what is wrong with a campaign's calendar,
// deterministically: posts outside the campaign's dates or on days it doesn't
// publish, crowded days and slots, long silences, periods short of the goal,
// posts due soon that aren't written or ready, posts of a type the campaign
// no longer includes, and posts from one idea or source published close
// together. Each finding names the posts and how it can be fixed, so the
// assistant offers the repair rather than only reporting the problem.
package calendarcheck

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/reschedule"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

// Thresholds of the checks.
const (
	// CrowdedPerDay is how many posts one platform may have on one day.
	CrowdedPerDay = 2
	// GapDays is the longest silence inside the campaign before it is one.
	GapDays = 7
	// DueSoonDays is how close a publish date is "soon".
	DueSoonDays = 3
	// SameIdeaDays is how far apart two posts from one idea must be on one
	// platform.
	SameIdeaDays = 7
	// SameSourceDays is how far apart two posts from one source must be on
	// one platform.
	SameSourceDays = 2
	// SlotMinutes is the least time between two posts to one account; a
	// campaign's wider spread widens it.
	SlotMinutes = 30
	// ConvertTypes caps the post types a finding offers to convert to.
	ConvertTypes = 3
)

// Codes of the findings.
const (
	CodeOutsideWindow = "outside_window"
	CodeDisabledDay   = "disabled_day"
	CodeCrowdedDay    = "crowded_day"
	CodeGap           = "gap"
	CodeUnwrittenSoon = "unwritten_soon"
	CodeNotReadySoon  = "not_ready_soon"
	// CodePostTypeNotAllowed is posts of a type the campaign no longer
	// includes on their platform.
	CodePostTypeNotAllowed = "post_type_not_allowed"
	CodeSameIdea           = "same_idea_close"
	CodeSameSource         = "same_source_close"
	CodeCrowdedSlot        = "crowded_slot"
	// CodeGoalShort is a goal period with fewer posts than the goal asks.
	CodeGoalShort = "goal_short"
	// CodeGoalOver is a goal period with well over the goal's posts.
	CodeGoalOver = "goal_over"
)

// Severity orders findings: warnings first.
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// FixKind names how a finding is repaired.
type FixKind string

const (
	// FixRedistribute evens the campaign's movable posts out over its dates.
	FixRedistribute FixKind = "redistribute"
	// FixPlanWindow plans ideas for an empty stretch.
	FixPlanWindow FixKind = "plan_window"
	// FixOpenPost opens a post to finish it.
	FixOpenPost FixKind = "open_post"
	// FixMovePost moves one post to another day.
	FixMovePost FixKind = "move_post"
	// FixEnablePostType allows a post type on a platform of the campaign
	// again.
	FixEnablePostType FixKind = "enable_post_type"
	// FixConvertPostType changes posts to another post type.
	FixConvertPostType FixKind = "convert_post_type"
	// FixUnschedule takes posts off the calendar.
	FixUnschedule FixKind = "unschedule"
	// FixDelete deletes posts.
	FixDelete FixKind = "delete"
)

// Fix is one way to repair a finding.
type Fix struct {
	Kind  FixKind `json:"kind"`
	Title string  `json:"title"`
	// PostID is the post an open_post fix opens or a move_post fix moves.
	PostID string `json:"postId,omitempty"`
	// PostIDs are the posts a convert, unschedule or delete fix changes.
	PostIDs []string `json:"postIds,omitempty"`
	// From and To bound a plan_window fix, YYYY-MM-DD; a move_post fix
	// moves its post from one day to the other.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Count is how many ideas a plan_window fix plans; zero leaves it to
	// the planner.
	Count int `json:"count,omitzero"`
	// PlatformID and PostType are the type an enable_post_type fix allows,
	// or the type a convert_post_type fix converts to (PostType only).
	PlatformID string `json:"platformId,omitempty"`
	PostType   string `json:"postType,omitempty"`
}

// Finding is one problem with the calendar.
type Finding struct {
	// Key identifies the finding across checks, so a dismissed one stays
	// dismissed while its posts are the same.
	Key      string   `json:"key"`
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	PostIDs  []string `json:"postIds,omitempty"`
	Fixes    []Fix    `json:"fixes"`
	// scope tells apart findings of one code over the same posts, such as
	// the goal's periods, in the key.
	scope string
}

// Check returns the campaign's calendar findings, warnings first. ideas
// maps a post to the ideas it was made from. now is the clock "soon" reads;
// a post's date is its day in the campaign's timezone, and the campaign's
// start and end are calendar dates already.
func Check(c *models.Campaign, posts []models.Post, ideas map[string][]string, now time.Time) []Finding {
	loc := Location(c)
	day := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	out := dateChecks(c, posts, day, day(now))
	cal := newCalendar(c, posts, day, day(now))
	out = append(out, postTypes(c, posts)...)
	out = append(out, cal.closeTogether(ideas)...)
	out = append(out, cal.crowdedSlots()...)
	out = append(out, cal.goal()...)
	for i := range out {
		f := &out[i]
		f.Key = key(c.ID, f.Code, f.PostIDs, f.Fixes, f.scope)
	}
	slices.SortStableFunc(out, bySeverity)
	return out
}

// dateChecks are the findings about the posts' dates: due soon and not
// ready, outside the campaign, on a day it doesn't publish, crowded days,
// and silences.
func dateChecks(c *models.Campaign, posts []models.Post, day func(time.Time) time.Time, today time.Time) []Finding {
	var out []Finding
	redistribute := Fix{Kind: FixRedistribute, Title: "Even out the calendar"}

	var start, end time.Time
	dated := c.StartDate != nil && c.EndDate != nil
	if dated {
		start, end = campaignphase.Date(*c.StartDate), campaignphase.Date(*c.EndDate)
	}
	enabled := scheduling.EnabledWeekdays(c.PublishingDays)

	var outside, offDay []string
	crowd := map[string][]string{}
	// movableCrowd marks the crowded days holding a post a redistribution
	// can move.
	movableCrowd := map[string]bool{}
	var scheduled []time.Time
	for _, p := range posts {
		if p.ScheduledAt == nil || p.Status == models.PostStatusPublished {
			continue
		}
		d := day(*p.ScheduledAt)
		scheduled = append(scheduled, d)
		movable := reschedule.Eligible(p.Status)
		if dated && movable && (d.Before(start) || d.After(end)) {
			outside = append(outside, p.ID)
		}
		if movable && len(enabled) > 0 && !enabled[d.Weekday()] {
			offDay = append(offDay, p.ID)
		}
		k := p.PlatformID + "|" + d.Format(time.DateOnly)
		crowd[k] = append(crowd[k], p.ID)
		movableCrowd[k] = movableCrowd[k] || movable

		if f, ok := dueSoon(p, d, today); ok {
			out = append(out, f)
		}
	}
	if len(outside) > 0 {
		out = append(out, Finding{Code: CodeOutsideWindow, Severity: SeverityWarning, PostIDs: sorted(outside),
			Message: fmt.Sprintf("%d post(s) are dated outside the campaign (%s to %s)", len(outside), start.Format("2 Jan"), end.Format("2 Jan")),
			Fixes:   []Fix{redistribute}})
	}
	if len(offDay) > 0 {
		out = append(out, Finding{Code: CodeDisabledDay, Severity: SeverityWarning, PostIDs: sorted(offDay),
			Message: fmt.Sprintf("%d post(s) fall on days the campaign doesn't publish (%s)", len(offDay), scheduling.DayLabels(c.PublishingDays)),
			Fixes:   []Fix{redistribute}})
	}
	out = append(out, crowded(crowd, movableCrowd, redistribute)...)
	if dated {
		for _, g := range gaps(start, end, scheduled) {
			out = append(out, Finding{Code: CodeGap, Severity: SeverityInfo,
				Message: fmt.Sprintf("nothing is planned from %s to %s", g[0].Format("2 Jan"), g[1].Format("2 Jan")),
				Fixes:   []Fix{{Kind: FixPlanWindow, Title: "Plan ideas for these days", From: g[0].Format(time.DateOnly), To: g[1].Format(time.DateOnly)}}})
		}
	}
	return out
}

// crowded is a finding per platform day holding more than CrowdedPerDay
// posts, in day order; crowd and movable are keyed "platform|YYYY-MM-DD". A
// day with no movable post gets no fix: a redistribution can't thin it.
func crowded(crowd map[string][]string, movable map[string]bool, fix Fix) []Finding {
	keys := make([]string, 0, len(crowd))
	for k := range crowd {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []Finding
	for _, k := range keys {
		ids := crowd[k]
		if len(ids) <= CrowdedPerDay {
			continue
		}
		platform, date, _ := strings.Cut(k, "|")
		f := Finding{Code: CodeCrowdedDay, Severity: SeverityWarning, PostIDs: sorted(ids),
			Message: fmt.Sprintf("%d posts for %s on %s", len(ids), cmp.Or(platform, "no platform"), date),
			Fixes:   []Fix{fix}}
		if !movable[k] {
			f.Message += "; they're all scheduled, so evening out the calendar can't move them: reschedule one by hand"
			f.Fixes = []Fix{}
		}
		out = append(out, f)
	}
	return out
}

// bySeverity puts warnings before the rest, keeping their order.
func bySeverity(a, b Finding) int {
	if a.Severity == b.Severity {
		return 0
	}
	if a.Severity == SeverityWarning {
		return -1
	}
	return 1
}

// dueSoon is the finding for a post publishing within DueSoonDays of today
// that isn't written (planned) or ready (draft).
func dueSoon(p models.Post, d, today time.Time) (Finding, bool) {
	until := d.Sub(today).Hours() / 24
	if until < 0 || until > DueSoonDays {
		return Finding{}, false
	}
	switch p.Status {
	case models.PostStatusPlanned:
		return Finding{Code: CodeUnwrittenSoon, Severity: SeverityWarning, PostIDs: []string{p.ID},
			Message: fmt.Sprintf("%q publishes %s but has no body yet", p.Title, d.Format("Mon 2 Jan")),
			Fixes:   []Fix{{Kind: FixOpenPost, Title: "Write it", PostID: p.ID}}}, true
	case models.PostStatusDraft:
		return Finding{Code: CodeNotReadySoon, Severity: SeverityWarning, PostIDs: []string{p.ID},
			Message: fmt.Sprintf("%q publishes %s but isn't marked ready", p.Title, d.Format("Mon 2 Jan")),
			Fixes:   []Fix{{Kind: FixOpenPost, Title: "Finish it", PostID: p.ID}}}, true
	}
	return Finding{}, false
}

// gaps are the stretches longer than GapDays inside [start, end] with no
// post, as [first empty day, last empty day].
func gaps(start, end time.Time, days []time.Time) [][2]time.Time {
	inside := []time.Time{start.AddDate(0, 0, -1)}
	for _, d := range days {
		if !d.Before(start) && !d.After(end) {
			inside = append(inside, d)
		}
	}
	inside = append(inside, end.AddDate(0, 0, 1))
	slices.SortFunc(inside, func(a, b time.Time) int { return a.Compare(b) })
	var out [][2]time.Time
	for i := 1; i < len(inside); i++ {
		from, to := inside[i-1].AddDate(0, 0, 1), inside[i].AddDate(0, 0, -1)
		if int(to.Sub(from).Hours()/24)+1 > GapDays {
			out = append(out, [2]time.Time{from, to})
		}
	}
	return out
}

// Location is the campaign's timezone, UTC when it has none or an unknown one.
func Location(c *models.Campaign) *time.Location {
	if c.Timezone == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(c.Timezone); err == nil {
		return loc
	}
	return time.UTC
}

func sorted(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// key is a finding's stable id: its campaign, its code, its posts, and its
// scope or else its fixes' windows. Only the first fix kinds count among
// the fixes: a later kind's dates move with the calendar, and the key must
// not.
func key(campaignID, code string, ids []string, fixes []Fix, scope string) string {
	h := sha256.New()
	h.Write([]byte(campaignID + "|" + code))
	for _, id := range ids {
		h.Write([]byte("|" + id))
	}
	if scope != "" {
		h.Write([]byte("|" + scope))
	} else {
		for _, f := range fixes {
			if f.Kind == FixRedistribute || f.Kind == FixPlanWindow || f.Kind == FixOpenPost {
				h.Write([]byte("|" + f.From + f.To))
			}
		}
	}
	return code + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}
