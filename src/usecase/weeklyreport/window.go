// Package weeklyreport builds the Monday email that tells a workspace's owners
// how the previous week went. Collect gathers the raw facts for one
// workspace's week, Render turns them into the display-ready view the email
// template lays out, and Empty decides whether the week is worth a message.
package weeklyreport

import (
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// SendHour is the local hour on Monday from which a workspace's report is due.
const SendHour = 9

// Window is the reported week in the workspace's zone: [Start, End) is the
// previous Monday to Monday, and [PrevStart, Start) the week before it.
type Window struct {
	PrevStart time.Time
	Start     time.Time
	End       time.Time
	Now       time.Time
	Loc       *time.Location
	Zone      string
}

// Due reports whether a workspace in loc should get its report at now, and
// the week it covers. A report is due from 09:00 local on Monday until the end
// of that Monday; a missed Monday is not caught up later.
func Due(now time.Time, loc *time.Location, zone string) (Window, bool) {
	local := now.In(loc)
	if local.Weekday() != time.Monday || local.Hour() < SendHour {
		return Window{}, false
	}
	y, m, d := local.AddDate(0, 0, -7).Date()
	return WeekOf(time.Date(y, m, d, 0, 0, 0, 0, time.UTC), loc, zone, now), true
}

// WeekOf is the window of the week that starts on date (its calendar date is
// read in UTC) in loc.
func WeekOf(date time.Time, loc *time.Location, zone string, now time.Time) Window {
	y, m, d := date.UTC().Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	return Window{
		PrevStart: start.AddDate(0, 0, -7),
		Start:     start,
		End:       start.AddDate(0, 0, 7),
		Now:       now,
		Loc:       loc,
		Zone:      zone,
	}
}

// StartDate is the week's first day as a YYYY-MM-DD string.
func (w Window) StartDate() string { return w.Start.Format(time.DateOnly) }

// Days returns the seven local midnights of the reported week.
func (w Window) Days() []time.Time {
	out := make([]time.Time, 7)
	for i := range out {
		out[i] = w.Start.AddDate(0, 0, i)
	}
	return out
}

// dayIndex returns which day of the week t falls on (0 = Monday), or -1 when t
// is outside the week.
func (w Window) dayIndex(t time.Time) int {
	if t.Before(w.Start) || !t.Before(w.End) {
		return -1
	}
	local := t.In(w.Loc)
	for i, d := range w.Days() {
		if !local.Before(d) && local.Before(d.AddDate(0, 0, 1)) {
			return i
		}
	}
	return -1
}

func (w Window) repo() repository.ReportWindow {
	return repository.ReportWindow{PrevStart: w.PrevStart, Start: w.Start, End: w.End, Now: w.Now}
}
