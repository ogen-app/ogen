// Package calendarcheck finds what is wrong with a campaign's calendar,
// deterministically: posts outside the campaign's dates or on days it doesn't
// publish, crowded days, long silences, and posts due soon that aren't
// written or ready. Each finding names the posts and how it can be fixed, so
// the assistant offers the repair rather than only reporting the problem.
package calendarcheck

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

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
)

// Codes of the findings.
const (
	CodeOutsideWindow = "outside_window"
	CodeDisabledDay   = "disabled_day"
	CodeCrowdedDay    = "crowded_day"
	CodeGap           = "gap"
	CodeUnwrittenSoon = "unwritten_soon"
	CodeNotReadySoon  = "not_ready_soon"
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
)

// Fix is one way to repair a finding.
type Fix struct {
	Kind  FixKind `json:"kind"`
	Title string  `json:"title"`
	// PostID is the post an open_post fix opens.
	PostID string `json:"postId,omitempty"`
	// From and To bound a plan_window fix, YYYY-MM-DD.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
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
}

// Check returns the campaign's calendar findings, warnings first. now is the
// clock "soon" reads; dates are the campaign's local days.
func Check(c *models.Campaign, posts []models.Post, now time.Time) []Finding {
	loc := location(c)
	day := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	var out []Finding
	add := func(f Finding) {
		f.Key = key(f.Code, f.PostIDs, f.Fixes)
		out = append(out, f)
	}
	redistribute := Fix{Kind: FixRedistribute, Title: "Even out the calendar"}

	var start, end time.Time
	dated := c.StartDate != nil && c.EndDate != nil
	if dated {
		start, end = day(*c.StartDate), day(*c.EndDate)
	}
	enabled := scheduling.EnabledWeekdays(c.PublishingDays)

	var outside, offDay []string
	crowd := map[string][]string{}
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

		if f, ok := dueSoon(p, d, day(now)); ok {
			add(f)
		}
	}
	if len(outside) > 0 {
		add(Finding{Code: CodeOutsideWindow, Severity: SeverityWarning, PostIDs: sorted(outside),
			Message: fmt.Sprintf("%d post(s) are dated outside the campaign (%s to %s)", len(outside), start.Format("2 Jan"), end.Format("2 Jan")),
			Fixes:   []Fix{redistribute}})
	}
	if len(offDay) > 0 {
		add(Finding{Code: CodeDisabledDay, Severity: SeverityWarning, PostIDs: sorted(offDay),
			Message: fmt.Sprintf("%d post(s) fall on days the campaign doesn't publish (%s)", len(offDay), scheduling.DayLabels(c.PublishingDays)),
			Fixes:   []Fix{redistribute}})
	}
	for _, f := range crowded(crowd, redistribute) {
		add(f)
	}
	if dated {
		for _, g := range gaps(start, end, scheduled) {
			add(Finding{Code: CodeGap, Severity: SeverityInfo,
				Message: fmt.Sprintf("nothing is planned from %s to %s", g[0].Format("2 Jan"), g[1].Format("2 Jan")),
				Fixes:   []Fix{{Kind: FixPlanWindow, Title: "Plan ideas for these days", From: g[0].Format(time.DateOnly), To: g[1].Format(time.DateOnly)}}})
		}
	}
	slices.SortStableFunc(out, bySeverity)
	return out
}

// crowded is a finding per platform day holding more than CrowdedPerDay
// posts, in day order; crowd is keyed "platform|YYYY-MM-DD".
func crowded(crowd map[string][]string, fix Fix) []Finding {
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
		out = append(out, Finding{Code: CodeCrowdedDay, Severity: SeverityWarning, PostIDs: sorted(ids),
			Message: fmt.Sprintf("%d posts for %s on %s", len(ids), cmp.Or(platform, "no platform"), date),
			Fixes:   []Fix{fix}})
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

func location(c *models.Campaign) *time.Location {
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

// key is a finding's stable id: its code, its posts and its fixes' windows.
func key(code string, ids []string, fixes []Fix) string {
	h := sha256.New()
	h.Write([]byte(code))
	for _, id := range ids {
		h.Write([]byte("|" + id))
	}
	for _, f := range fixes {
		h.Write([]byte("|" + f.From + f.To))
	}
	return code + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}
