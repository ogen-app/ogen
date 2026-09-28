// Package scheduling holds the pure, dependency-free helpers that turn a
// campaign's scheduling settings — publishing time, timezone,
// publishing days, and spread — into a concrete publish instant. It is shared
// by the campaigns handler (validation + defaults) and the content-plan flow
// (composing each generated draft's scheduled_at), so the two never drift.
package scheduling

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Defaults applied when a campaign leaves a scheduling field unset.
const (
	DefaultPublishingTime = "09:00"
	DefaultSpreadMinutes  = 15
	// MaxSpreadMinutes caps the jitter at ±12h — generous, guards the column.
	MaxSpreadMinutes = 720
)

// AllWeekdayTokens is the canonical set and week order of publishing-day tokens.
var AllWeekdayTokens = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var tokenToWeekday = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

var weekdayLabel = map[string]string{
	"mon": "Mon", "tue": "Tue", "wed": "Wed", "thu": "Thu",
	"fri": "Fri", "sat": "Sat", "sun": "Sun",
}

// DefaultPublishingDays returns a fresh copy of the all-days default.
func DefaultPublishingDays() []string {
	out := make([]string, len(AllWeekdayTokens))
	copy(out, AllWeekdayTokens)
	return out
}

// ValidWeekday reports whether token is a known weekday token (case-insensitive).
func ValidWeekday(token string) bool {
	_, ok := tokenToWeekday[strings.ToLower(strings.TrimSpace(token))]
	return ok
}

// ValidClock reports whether s is a valid zero-padded "HH:MM" 24-hour time.
func ValidClock(s string) bool {
	_, _, ok := parseClock(s)
	return ok
}

// parseClock parses a zero-padded "HH:MM" 24-hour string.
func parseClock(s string) (hh, mm int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// EnabledWeekdays parses day tokens into a set. An empty or fully-unparseable
// list falls back to all seven days, so scheduling is never blocked.
func EnabledWeekdays(days []string) map[time.Weekday]bool {
	set := make(map[time.Weekday]bool, len(days))
	for _, d := range days {
		if wd, ok := tokenToWeekday[strings.ToLower(strings.TrimSpace(d))]; ok {
			set[wd] = true
		}
	}
	if len(set) == 0 {
		for _, wd := range tokenToWeekday {
			set[wd] = true
		}
	}
	return set
}

// DayLabels renders the enabled days as "Mon, Wed, Fri" in week order, or ""
// when all seven are enabled (nothing to restrict the model to).
func DayLabels(days []string) string {
	set := EnabledWeekdays(days)
	if len(set) >= 7 {
		return ""
	}
	out := make([]string, 0, len(set))
	for _, tok := range AllWeekdayTokens {
		if set[tokenToWeekday[tok]] {
			out = append(out, weekdayLabel[tok])
		}
	}
	return strings.Join(out, ", ")
}

// dateOnly strips the time-of-day, anchoring the date in UTC for weekday math.
func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// SnapToPublishingDay moves date to the nearest enabled publishing day within
// [start, end], preferring later days at equal distance. found is false only
// when the window contains no enabled weekday at all — the caller then keeps the
// original date and warns.
func SnapToPublishingDay(date, start, end time.Time, enabled map[time.Weekday]bool) (snapped time.Time, found bool) {
	date, start, end = dateOnly(date), dateOnly(start), dateOnly(end)
	if enabled[date.Weekday()] {
		return date, true
	}
	for d := 1; d <= 6; d++ {
		if fwd := date.AddDate(0, 0, d); !fwd.After(end) && enabled[fwd.Weekday()] {
			return fwd, true
		}
		if bwd := date.AddDate(0, 0, -d); !bwd.Before(start) && enabled[bwd.Weekday()] {
			return bwd, true
		}
	}
	return date, false
}

// SpreadOffset returns a deterministic minute offset in [-spread, +spread]
// derived from the post id, so posts around the same time fan out without an
// RNG (stable across re-runs). spread <= 0 yields 0.
func SpreadOffset(postID string, spread int) int {
	if spread <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(postID))
	return int(h.Sum32()%uint32(2*spread+1)) - spread
}

// ComposeScheduledAt turns the model's publishDate (YYYY-MM-DD) into a post's
// absolute UTC scheduled_at using the campaign's scheduling settings: snap to an
// enabled publishing day, place it at clock in loc, then nudge it within the
// spread window. It returns the effective (possibly snapped) date string and
// noEnabledDay=true when the campaign window had no enabled weekday (the date is
// kept, only the time-of-day is applied). A malformed publishDate yields a nil
// instant and the original string.
func ComposeScheduledAt(publishDate, postID string, loc *time.Location, clock string, days []string, spread int, start, end *time.Time) (at *time.Time, effectiveDate string, noEnabledDay bool) {
	date, err := time.Parse("2006-01-02", publishDate)
	if err != nil {
		return nil, publishDate, false
	}
	if loc == nil {
		loc = time.UTC
	}

	lo, hi := start, end
	// Fall back to a ±7-day window around the date when the campaign has no
	// bounds (shouldn't happen post-validation) so snapping still resolves.
	loBound := date.AddDate(0, 0, -7)
	hiBound := date.AddDate(0, 0, 7)
	if lo != nil {
		loBound = *lo
	}
	if hi != nil {
		hiBound = *hi
	}

	day, found := SnapToPublishingDay(date, loBound, hiBound, EnabledWeekdays(days))

	hh, mm, ok := parseClock(clock)
	if !ok {
		hh, mm = 9, 0
	}
	y, mo, d := day.Date()
	local := time.Date(y, mo, d, hh, mm, 0, 0, loc)
	// Keep the jitter on the selected day. A publishing time near midnight plus a
	// large spread could otherwise push the instant into an adjacent calendar
	// date — landing on a day outside PublishingDays or the campaign bounds, and
	// disagreeing with the effectiveDate we return. Clamp to [00:00, 23:59] of the
	// day in loc so only the on-day portion of the jitter is retained.
	adjusted := local.Add(time.Duration(SpreadOffset(postID, spread)) * time.Minute)
	if dayStart := time.Date(y, mo, d, 0, 0, 0, 0, loc); adjusted.Before(dayStart) {
		adjusted = dayStart
	} else if dayEnd := time.Date(y, mo, d, 23, 59, 0, 0, loc); adjusted.After(dayEnd) {
		adjusted = dayEnd
	}
	t := adjusted.UTC()
	return &t, day.Format("2006-01-02"), !found
}
