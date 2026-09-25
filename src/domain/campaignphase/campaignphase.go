// Package campaignphase owns a campaign's per-phase date plan (CON-166): which
// calendar days each phase of the campaign's type covers. It is the single
// implementation of the "split the campaign timeline across its phases" rule
// that content_plan, the campaign assistant's current-phase lookup and the
// reschedule action previously each re-implemented, so the API and every
// consumer agree on the windows.
//
// A plan is either derived (the default: [start_date, end_date] split evenly
// across the phases, remainder days to the earliest) or manual (a user-edited
// plan stored whole in campaign_phase_windows). A stored plan is honoured only
// while it is still valid for the campaign's current type and dates; otherwise
// the derived plan applies, so a stale plan can never mis-date content.
package campaignphase

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Source says where a campaign's phase windows come from.
type Source string

const (
	// SourceDerived: the even split of the campaign's dates (no stored plan, or
	// the stored one no longer fits the campaign).
	SourceDerived Source = "derived"
	// SourceManual: a user-edited plan from campaign_phase_windows.
	SourceManual Source = "manual"
	// SourceUnscheduled: the campaign has no start/end date, so no windows.
	SourceUnscheduled Source = "unscheduled"
)

// Window is one phase's inclusive calendar-day range. Start and End are dates
// at 00:00 UTC.
type Window struct {
	Phase models.CampaignTypePhase
	Start time.Time
	End   time.Time
}

// Contains reports whether t's calendar date falls inside the window.
func (w Window) Contains(t time.Time) bool {
	d := Date(t)
	return !d.Before(w.Start) && !d.After(w.End)
}

// Date truncates t to its calendar date at 00:00 UTC — the normalisation every
// window computation runs on.
func Date(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// SortedPhases returns the campaign type's phases in sequence order (nil when
// the type isn't hydrated).
func SortedPhases(c *models.Campaign) []models.CampaignTypePhase {
	if c == nil || c.CampaignType == nil {
		return nil
	}
	phases := slices.Clone(c.CampaignType.Phases)
	slices.SortStableFunc(phases, func(a, b models.CampaignTypePhase) int { return cmp.Compare(a.Sequence, b.Sequence) })
	return phases
}

// Split partitions the closed interval [start, end] into n contiguous
// inclusive day windows in chronological order, remainder days to the earliest
// windows. When n exceeds the day count every window is the full range (the
// alternative — zero-day windows — would yield invalid publish dates). The
// arithmetic runs on the given instants, so callers wanting calendar semantics
// pass Date()-normalised bounds.
func Split(start, end time.Time, n int) [][2]time.Time {
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return [][2]time.Time{{start, end}}
	}
	out := make([][2]time.Time, n)
	totalDays := max(int(end.Sub(start).Hours()/24)+1, 1)
	if totalDays < n {
		for i := range out {
			out[i] = [2]time.Time{start, end}
		}
		return out
	}
	base, rem := totalDays/n, totalDays%n
	cursor := start
	for i := range n {
		d := base
		if i < rem {
			d++
		}
		winEnd := cursor.AddDate(0, 0, d-1)
		out[i] = [2]time.Time{cursor, winEnd}
		cursor = winEnd.AddDate(0, 0, 1)
	}
	return out
}

// Derive returns the default plan: the campaign's dates split evenly across
// its phases. Nil when the campaign is unscheduled or has no phases.
func Derive(c *models.Campaign) []Window {
	phases := SortedPhases(c)
	if c == nil || c.StartDate == nil || c.EndDate == nil || len(phases) == 0 {
		return nil
	}
	spans := Split(Date(*c.StartDate), Date(*c.EndDate), len(phases))
	out := make([]Window, len(phases))
	for i, ph := range phases {
		out[i] = Window{Phase: ph, Start: spans[i][0], End: spans[i][1]}
	}
	return out
}

// Resolve returns the campaign's effective phase windows and their source: the
// stored plan (c.PhaseWindows) when it is still valid for the campaign's
// current type and dates, else the derived plan. The campaign must be hydrated
// with its type (and, for manual plans, its PhaseWindows).
func Resolve(c *models.Campaign) ([]Window, Source) {
	if c == nil || c.StartDate == nil || c.EndDate == nil {
		return nil, SourceUnscheduled
	}
	if len(c.PhaseWindows) > 0 {
		entries := make([]Entry, len(c.PhaseWindows))
		for i, w := range c.PhaseWindows {
			entries[i] = Entry{PhaseID: w.PhaseID, Start: w.StartDate, End: w.EndDate}
		}
		if windows, err := Validate(c, entries); err == nil {
			return windows, SourceManual
		}
	}
	return Derive(c), SourceDerived
}

// PhaseAt returns the phase whose effective window contains t. Before the
// campaign starts it is the first phase, after it ends the last; an
// unscheduled campaign falls back to the first phase. ok is false only when
// the campaign type has no phases.
func PhaseAt(c *models.Campaign, t time.Time) (models.CampaignTypePhase, bool) {
	phases := SortedPhases(c)
	if len(phases) == 0 {
		return models.CampaignTypePhase{}, false
	}
	windows, _ := Resolve(c)
	if len(windows) == 0 {
		return phases[0], true
	}
	d := Date(t)
	if d.Before(windows[0].Start) {
		return windows[0].Phase, true
	}
	for _, w := range windows {
		if w.Contains(d) {
			return w.Phase, true
		}
	}
	return windows[len(windows)-1].Phase, true
}

// Entry is one phase's requested window in a manual plan.
type Entry struct {
	PhaseID string
	Start   time.Time
	End     time.Time
}

// PlanError is a manual plan that breaks the plan rules. Its message names the
// first offending phase and is safe to return to the client.
type PlanError struct{ Msg string }

func (e *PlanError) Error() string { return e.Msg }

// ErrUnscheduled is returned by Validate when the campaign has no dates, so no
// plan can be anchored to it.
var ErrUnscheduled = &PlanError{Msg: "campaign has no start_date/end_date; set them before editing phase dates"}

// Validate checks a manual plan against the campaign's current type and dates
// and returns it as windows in sequence order. The rules: exactly one window
// per phase of the campaign's type; windows contiguous in sequence order (each
// starts the day after the previous ends — no gaps, no overlaps); the first
// starts on start_date and the last ends on end_date; each spans at least one
// day. Entry dates are normalised to calendar dates.
func Validate(c *models.Campaign, entries []Entry) ([]Window, error) {
	if c == nil || c.StartDate == nil || c.EndDate == nil {
		return nil, ErrUnscheduled
	}
	phases := SortedPhases(c)
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if _, dup := byID[e.PhaseID]; dup {
			return nil, &PlanError{Msg: fmt.Sprintf("phase %q appears more than once", e.PhaseID)}
		}
		byID[e.PhaseID] = e
	}
	known := make(map[string]bool, len(phases))
	for _, ph := range phases {
		known[ph.ID] = true
	}
	for _, e := range entries {
		if !known[e.PhaseID] {
			return nil, &PlanError{Msg: fmt.Sprintf("phase %q is not a phase of this campaign's type", e.PhaseID)}
		}
	}

	start, end := Date(*c.StartDate), Date(*c.EndDate)
	out := make([]Window, 0, len(phases))
	for i, ph := range phases {
		e, ok := byID[ph.ID]
		if !ok {
			return nil, &PlanError{Msg: fmt.Sprintf("phase %q is missing from the plan", ph.Name)}
		}
		ws, we := Date(e.Start), Date(e.End)
		if we.Before(ws) {
			return nil, &PlanError{Msg: fmt.Sprintf("phase %q ends before it starts", ph.Name)}
		}
		if i == 0 && !ws.Equal(start) {
			return nil, &PlanError{Msg: fmt.Sprintf("phase %q must start on the campaign start date (%s)", ph.Name, start.Format(time.DateOnly))}
		}
		if i > 0 {
			if want := out[i-1].End.AddDate(0, 0, 1); !ws.Equal(want) {
				return nil, &PlanError{Msg: fmt.Sprintf("phase %q must start on %s, the day after the previous phase ends (no gaps or overlaps)", ph.Name, want.Format(time.DateOnly))}
			}
		}
		if i == len(phases)-1 && !we.Equal(end) {
			return nil, &PlanError{Msg: fmt.Sprintf("phase %q must end on the campaign end date (%s)", ph.Name, end.Format(time.DateOnly))}
		}
		out = append(out, Window{Phase: ph, Start: ws, End: we})
	}
	return out, nil
}

// Rebound re-anchors a stored manual plan to new campaign dates after a date
// change: the first window's start moves to the new start_date and the last
// window's end to the new end_date, inner boundaries stay. It returns the
// re-anchored entries when they still form a valid plan, or ok=false when the
// plan can't survive (a window emptied/inverted, the phase set changed, or the
// campaign lost its dates) — the caller then drops it back to derived.
func Rebound(c *models.Campaign, stored []models.CampaignPhaseWindow) ([]Entry, bool) {
	if c == nil || c.StartDate == nil || c.EndDate == nil || len(stored) == 0 {
		return nil, false
	}
	seq := make(map[string]int, len(stored))
	for _, ph := range SortedPhases(c) {
		seq[ph.ID] = ph.Sequence
	}
	entries := make([]Entry, 0, len(stored))
	for _, w := range stored {
		if _, ok := seq[w.PhaseID]; !ok {
			return nil, false
		}
		entries = append(entries, Entry{PhaseID: w.PhaseID, Start: w.StartDate, End: w.EndDate})
	}
	slices.SortStableFunc(entries, func(a, b Entry) int { return cmp.Compare(seq[a.PhaseID], seq[b.PhaseID]) })
	entries[0].Start = Date(*c.StartDate)
	entries[len(entries)-1].End = Date(*c.EndDate)
	if _, err := Validate(c, entries); err != nil {
		return nil, false
	}
	return entries, true
}
