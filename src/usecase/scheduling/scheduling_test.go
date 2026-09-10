package scheduling

import (
	"strconv"
	"testing"
	"time"
)

var wdToken = map[time.Weekday]string{
	time.Monday: "mon", time.Tuesday: "tue", time.Wednesday: "wed", time.Thursday: "thu",
	time.Friday: "fri", time.Saturday: "sat", time.Sunday: "sun",
}

func TestValidClock(t *testing.T) {
	cases := map[string]bool{
		"09:00": true, "00:00": true, "23:59": true,
		"9:00": false, "24:00": false, "12:60": false, "": false, "0900": false, "09:0": false,
	}
	for in, want := range cases {
		if got := ValidClock(in); got != want {
			t.Errorf("ValidClock(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValidWeekday(t *testing.T) {
	for _, ok := range []string{"mon", "MON", " tue ", "sun"} {
		if !ValidWeekday(ok) {
			t.Errorf("ValidWeekday(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"monday", "xyz", "", "8"} {
		if ValidWeekday(bad) {
			t.Errorf("ValidWeekday(%q) = true, want false", bad)
		}
	}
}

func TestEnabledWeekdays(t *testing.T) {
	if got := EnabledWeekdays(nil); len(got) != 7 {
		t.Errorf("empty → %d days, want 7 (fallback)", len(got))
	}
	if got := EnabledWeekdays([]string{"bogus"}); len(got) != 7 {
		t.Errorf("all-invalid → %d days, want 7 (fallback)", len(got))
	}
	got := EnabledWeekdays([]string{"mon", "wed"})
	if !got[time.Monday] || !got[time.Wednesday] || got[time.Tuesday] || len(got) != 2 {
		t.Errorf("['mon','wed'] → %v", got)
	}
}

func TestDayLabels(t *testing.T) {
	if got := DayLabels(AllWeekdayTokens); got != "" {
		t.Errorf("all days → %q, want empty", got)
	}
	if got := DayLabels([]string{"fri", "mon", "wed"}); got != "Mon, Wed, Fri" {
		t.Errorf("subset → %q, want 'Mon, Wed, Fri' (week order)", got)
	}
}

func TestSnapToPublishingDay(t *testing.T) {
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	start, end := date.AddDate(0, 0, -7), date.AddDate(0, 0, 7)

	// Already on an enabled day → unchanged.
	if got, found := SnapToPublishingDay(date, start, end, map[time.Weekday]bool{date.Weekday(): true}); !found || !got.Equal(date) {
		t.Errorf("on-enabled: got %v found %v, want %v true", got, found, date)
	}

	// Disabled, next day enabled → snap forward one day.
	next := map[time.Weekday]bool{(date.Weekday() + 1) % 7: true}
	if got, found := SnapToPublishingDay(date, start, end, next); !found || !got.Equal(date.AddDate(0, 0, 1)) {
		t.Errorf("snap-forward: got %v found %v", got, found)
	}

	// Equal distance both sides → forward preferred.
	both := map[time.Weekday]bool{(date.Weekday() + 1) % 7: true, (date.Weekday() + 6) % 7: true}
	if got, _ := SnapToPublishingDay(date, start, end, both); !got.Equal(date.AddDate(0, 0, 1)) {
		t.Errorf("equal-distance: got %v, want forward %v", got, date.AddDate(0, 0, 1))
	}

	// Single-day window on a disabled day → no enabled day found.
	if got, found := SnapToPublishingDay(date, date, date, next); found || !got.Equal(date) {
		t.Errorf("no-enabled-in-window: got %v found %v, want %v false", got, found, date)
	}
}

func TestSpreadOffset(t *testing.T) {
	if got := SpreadOffset("anything", 0); got != 0 {
		t.Errorf("spread 0 → %d, want 0", got)
	}
	// Deterministic + bounded.
	a, b := SpreadOffset("post-123", 30), SpreadOffset("post-123", 30)
	if a != b {
		t.Errorf("not deterministic: %d vs %d", a, b)
	}
	if a < -30 || a > 30 {
		t.Errorf("offset %d out of ±30", a)
	}
	// Different ids should generally differ (guards a constant-hash bug).
	if SpreadOffset("a", 30) == SpreadOffset("b", 30) && SpreadOffset("c", 30) == SpreadOffset("d", 30) {
		t.Error("offsets look constant across ids")
	}
}

func TestComposeScheduledAt(t *testing.T) {
	loc := time.FixedZone("UTC+2", 2*3600)
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	start, end := date.AddDate(0, 0, -7), date.AddDate(0, 0, 7)
	ds := date.Format("2006-01-02")

	// Enabled day, no jitter → date @ 09:00 in loc, as UTC (07:00Z).
	at, eff, no := ComposeScheduledAt(ds, "p1", loc, "09:00", []string{wdToken[date.Weekday()]}, 0, &start, &end)
	want := time.Date(2026, 8, 12, 9, 0, 0, 0, loc).UTC()
	if at == nil || !at.Equal(want) {
		t.Errorf("compose = %v, want %v", at, want)
	}
	if eff != ds || no {
		t.Errorf("effectiveDate=%q noEnabledDay=%v, want %q false", eff, no, ds)
	}

	// Disabled day, only next weekday enabled → snapped forward; effectiveDate moves.
	at2, eff2, _ := ComposeScheduledAt(ds, "p1", loc, "09:00", []string{wdToken[(date.Weekday()+1)%7]}, 0, &start, &end)
	wantDay := date.AddDate(0, 0, 1)
	if eff2 != wantDay.Format("2006-01-02") {
		t.Errorf("snap effectiveDate=%q, want %q", eff2, wantDay.Format("2006-01-02"))
	}
	if at2 == nil || !at2.Equal(time.Date(2026, 8, 13, 9, 0, 0, 0, loc).UTC()) {
		t.Errorf("snapped instant = %v", at2)
	}

	// Malformed date → nil instant, original string echoed.
	if at3, eff3, _ := ComposeScheduledAt("not-a-date", "p1", loc, "09:00", nil, 0, &start, &end); at3 != nil || eff3 != "not-a-date" {
		t.Errorf("malformed → %v %q, want nil 'not-a-date'", at3, eff3)
	}

	// Jitter deterministic + within window.
	j1, _, _ := ComposeScheduledAt(ds, "same-id", loc, "09:00", []string{wdToken[date.Weekday()]}, 30, &start, &end)
	j2, _, _ := ComposeScheduledAt(ds, "same-id", loc, "09:00", []string{wdToken[date.Weekday()]}, 30, &start, &end)
	if !j1.Equal(*j2) {
		t.Errorf("jitter not deterministic: %v vs %v", j1, j2)
	}
	if d := j1.Sub(want).Minutes(); d < -30 || d > 30 {
		t.Errorf("jitter drift %.0f min out of ±30", d)
	}
}

func TestComposeScheduledAt_SpreadClampedToDay(t *testing.T) {
	loc := time.UTC
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC) // days=nil → all enabled → no snap
	start, end := date.AddDate(0, 0, -7), date.AddDate(0, 0, 7)
	ds := date.Format("2006-01-02")

	dayStart := time.Date(2026, 8, 12, 0, 0, 0, 0, loc).UTC()
	dayEnd := time.Date(2026, 8, 12, 23, 59, 0, 0, loc).UTC()

	// Force one id to a negative offset and one to a positive offset so both
	// boundary directions are exercised deterministically.
	var negID, posID string
	for i := 0; negID == "" || posID == ""; i++ {
		if i > 10000 {
			t.Fatal("could not find both signed offsets")
		}
		id := "post-" + strconv.Itoa(i)
		switch off := SpreadOffset(id, MaxSpreadMinutes); {
		case off < 0 && negID == "":
			negID = id
		case off > 0 && posID == "":
			posID = id
		}
	}

	// Negative spread at 00:00 must clamp UP to 00:00, not roll into the prior day.
	atNeg, effNeg, _ := ComposeScheduledAt(ds, negID, loc, "00:00", nil, MaxSpreadMinutes, &start, &end)
	if atNeg == nil || !atNeg.Equal(dayStart) {
		t.Errorf("00:00 with negative spread = %v, want clamped to %v", atNeg, dayStart)
	}
	if effNeg != ds {
		t.Errorf("negative-spread effectiveDate = %q, want %q (must not cross to prior day)", effNeg, ds)
	}

	// Positive spread at 23:59 must clamp DOWN to 23:59, not roll into the next day.
	atPos, effPos, _ := ComposeScheduledAt(ds, posID, loc, "23:59", nil, MaxSpreadMinutes, &start, &end)
	if atPos == nil || !atPos.Equal(dayEnd) {
		t.Errorf("23:59 with positive spread = %v, want clamped to %v", atPos, dayEnd)
	}
	if effPos != ds {
		t.Errorf("positive-spread effectiveDate = %q, want %q (must not cross to next day)", effPos, ds)
	}

	// Invariant across many ids at both boundary clocks: the instant never leaves
	// the day and effectiveDate always agrees.
	for i := range 300 {
		id := "x-" + strconv.Itoa(i)
		for _, clock := range []string{"00:00", "23:59"} {
			at, eff, _ := ComposeScheduledAt(ds, id, loc, clock, nil, MaxSpreadMinutes, &start, &end)
			if at == nil || at.Before(dayStart) || at.After(dayEnd) {
				t.Fatalf("clock %s id %s → %v, out of [%v, %v]", clock, id, at, dayStart, dayEnd)
			}
			if eff != ds {
				t.Fatalf("clock %s id %s → effectiveDate %q, want %q", clock, id, eff, ds)
			}
		}
	}
}
