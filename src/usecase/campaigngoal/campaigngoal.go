// Package campaigngoal turns a campaign's post-rate goal — a per-period count
// (estimated_post_count) plus a cadence (week|month) — into the two things the
// rest of the app needs: the effective total number of posts to generate (the
// CON-182 content-plan input) and the per-period progress windows (the campaign
// overview's goal recap).
//
// All calendar math runs in a caller-supplied *time.Location so that a goal of
// "5 posts per week" lines up with how CON-181 stamps each draft's scheduled_at
// in the campaign timezone. The package itself is pure (stdlib only); callers
// resolve the location via settings.ResolveTimezone and pass it in.
package campaigngoal

import (
	"fmt"
	"time"
)

// Supported goal cadences and the default applied to an unset value. estimated_post_count
// is interpreted as "this many posts per one of these periods".
const (
	CadenceWeek    = "week"
	CadenceMonth   = "month"
	DefaultCadence = CadenceMonth
)

// ValidCadence reports whether s is a supported cadence.
func ValidCadence(s string) bool {
	return s == CadenceWeek || s == CadenceMonth
}

// Normalize returns the effective cadence: DefaultCadence for an empty string,
// the value itself when valid, and a 400-worthy error otherwise.
func Normalize(s string) (string, error) {
	switch s {
	case "":
		return DefaultCadence, nil
	case CadenceWeek, CadenceMonth:
		return s, nil
	default:
		return "", fmt.Errorf("goal_cadence must be %q or %q, got %q", CadenceWeek, CadenceMonth, s)
	}
}

// Periods is the number of cadence periods the [start, end] window spans,
// rounding any partial trailing period UP (ceil). Bounds are read as calendar
// days in loc. It returns 1 for a missing/empty window or an unrecognized
// cadence, so a caller can safely treat the per-period count as an absolute
// total in those cases.
func Periods(cadence string, start, end *time.Time, loc *time.Location) int {
	if start == nil || end == nil {
		return 1
	}
	if loc == nil {
		loc = time.UTC
	}
	s := dateOnly(*start, loc)
	e := dateOnly(*end, loc)
	if e.Before(s) {
		return 1
	}
	switch cadence {
	case CadenceWeek:
		return ceilDiv(daysInclusive(s, e), 7)
	case CadenceMonth:
		return monthIndex(e) - monthIndex(s) + 1
	default:
		return 1
	}
}

// EffectiveCount is the total number of posts the goal implies over the whole
// campaign: perPeriod × Periods. It returns 0 when perPeriod is unset or
// non-positive, letting the content-plan flow fall back to a model-decided
// count (its behavior before goals existed).
func EffectiveCount(perPeriod *int, cadence string, start, end *time.Time, loc *time.Location) int {
	if perPeriod == nil || *perPeriod <= 0 {
		return 0
	}
	return *perPeriod * Periods(cadence, start, end, loc)
}

// Window is one goal period: a half-open [Start, End) instant range in loc plus
// a display label ("Week 1", "Aug 2026").
type Window struct {
	Start time.Time
	End   time.Time
	Label string
}

// Windows returns the ordered goal periods that partition [start, end] at the
// given cadence, in loc. Weekly windows are successive 7-day spans anchored at
// start; monthly windows follow calendar months. The first window starts at the
// campaign start and the last ends at the day after the campaign end, so the
// windows exactly tile the campaign span with no gaps or overlap. Returns nil
// when the window is missing/invalid or the cadence is unrecognized.
func Windows(cadence string, start, end *time.Time, loc *time.Location) []Window {
	if start == nil || end == nil {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}
	s := dateOnly(*start, loc)
	e := dateOnly(*end, loc)
	if e.Before(s) {
		return nil
	}
	endExclusive := e.AddDate(0, 0, 1) // the day after the last campaign day

	switch cadence {
	case CadenceWeek:
		n := ceilDiv(daysInclusive(s, e), 7)
		out := make([]Window, 0, n)
		for i := range n {
			ws := s.AddDate(0, 0, 7*i)
			we := s.AddDate(0, 0, 7*(i+1))
			if we.After(endExclusive) {
				we = endExclusive // clamp the trailing partial week to the campaign end
			}
			out = append(out, Window{Start: ws, End: we, Label: fmt.Sprintf("Week %d", i+1)})
		}
		return out
	case CadenceMonth:
		n := monthIndex(e) - monthIndex(s) + 1
		firstOfStartMonth := time.Date(s.Year(), s.Month(), 1, 0, 0, 0, 0, loc)
		out := make([]Window, 0, n)
		for i := range n {
			monthStart := firstOfStartMonth.AddDate(0, i, 0)
			ws := monthStart
			if ws.Before(s) {
				ws = s // clamp the first window to the campaign start
			}
			we := firstOfStartMonth.AddDate(0, i+1, 0)
			if we.After(endExclusive) {
				we = endExclusive // clamp the last window to the campaign end
			}
			out = append(out, Window{Start: ws, End: we, Label: monthStart.Format("Jan 2006")})
		}
		return out
	default:
		return nil
	}
}

// dateOnly is the calendar day of t in loc, at midnight.
func dateOnly(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// daysInclusive counts calendar days from s to e inclusive (both midnights).
// It measures the span in UTC so DST-shortened/lengthened days don't skew the
// count.
func daysInclusive(s, e time.Time) int {
	su := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, time.UTC)
	eu := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC)
	return int(eu.Sub(su).Hours()/24) + 1
}

// monthIndex maps a time to a monotonically increasing month number so month
// differences are a simple subtraction.
func monthIndex(t time.Time) int { return t.Year()*12 + int(t.Month()) - 1 }

func ceilDiv(a, b int) int {
	if a <= 0 || b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
