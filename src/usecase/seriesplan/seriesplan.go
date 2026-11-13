// Package seriesplan is the plan arithmetic of a campaign's run of a series:
// which days its posts fall on, and so how many plan slots it claims. It reads
// the campaign window in the campaign timezone, on the same periods as the
// goal (campaigngoal.Windows), so a series and the goal count the same weeks
// and months.
package seriesplan

import (
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
)

// Days is the calendar day, at midnight in loc, of each post a run at r makes
// over [start, end], in order. It runs in the first period and then one of
// every r.Interval() periods. With a weekday its one post in a period goes on
// that day, and a trailing part-period without that day gets none; without one
// its posts are spread evenly over the period's days. A nil rhythm
// (occasional), an invalid one, or a missing window has none.
func Days(r *models.SeriesRhythm, start, end *time.Time, loc *time.Location) []time.Time {
	if r == nil || !r.Valid() {
		return nil
	}
	windows := campaigngoal.Windows(r.Per, start, end, loc)
	day, onDay := r.Day()
	var out []time.Time
	for i, w := range windows {
		if i%r.Interval() != 0 {
			continue
		}
		if onDay {
			// A weekly window is at most seven days, so it holds the day once.
			for d := w.Start; d.Before(w.End); d = d.AddDate(0, 0, 1) {
				if d.Weekday() == day {
					out = append(out, d)
					break
				}
			}
			continue
		}
		n := days(w)
		for k := range r.Times {
			out = append(out, w.Start.AddDate(0, 0, k*n/r.Times))
		}
	}
	return out
}

// Claim is how many plan slots a run at r takes over [start, end]: one per
// day in Days. Occasional claims none.
func Claim(r *models.SeriesRhythm, start, end *time.Time, loc *time.Location) int {
	return len(Days(r, start, end, loc))
}

// days counts the calendar days in w, measured in UTC so a DST change inside
// the window doesn't skew it.
func days(w campaigngoal.Window) int {
	s := time.Date(w.Start.Year(), w.Start.Month(), w.Start.Day(), 0, 0, 0, 0, time.UTC)
	e := time.Date(w.End.Year(), w.End.Month(), w.End.Day(), 0, 0, 0, 0, time.UTC)
	return int(e.Sub(s).Hours() / 24)
}
