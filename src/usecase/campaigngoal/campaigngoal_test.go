package campaigngoal

import (
	"testing"
	"time"
)

func d(y int, m time.Month, day int) *time.Time {
	t := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	return &t
}

//go:fix inline

func TestNormalize(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", CadenceMonth, false},
		{"week", CadenceWeek, false},
		{"month", CadenceMonth, false},
		{"day", "", true},
		{"Week", "", true}, // case-sensitive; handler trims but does not lowercase
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Normalize(%q) expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Normalize(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPeriods(t *testing.T) {
	cases := []struct {
		name    string
		cadence string
		start   *time.Time
		end     *time.Time
		want    int
	}{
		{"week 2-month span rounds up", CadenceWeek, d(2026, 6, 1), d(2026, 7, 31), 9}, // ceil(61/7)
		{"month 2-month span", CadenceMonth, d(2026, 6, 1), d(2026, 7, 31), 2},
		{"week partial month", CadenceWeek, d(2026, 9, 1), d(2026, 9, 20), 3}, // ceil(20/7)
		{"month single month", CadenceMonth, d(2026, 9, 1), d(2026, 9, 20), 1},
		{"week single day", CadenceWeek, d(2026, 9, 1), d(2026, 9, 1), 1},
		{"month across year boundary", CadenceMonth, d(2025, 12, 15), d(2026, 2, 3), 3},
		{"missing dates → 1", CadenceWeek, nil, nil, 1},
		{"end before start → 1", CadenceWeek, d(2026, 9, 10), d(2026, 9, 1), 1},
		{"unknown cadence → 1", "day", d(2026, 6, 1), d(2026, 7, 31), 1},
	}
	for _, c := range cases {
		if got := Periods(c.cadence, c.start, c.end, time.UTC); got != c.want {
			t.Errorf("%s: Periods = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestEffectiveCount(t *testing.T) {
	cases := []struct {
		name      string
		perPeriod *int
		cadence   string
		start     *time.Time
		end       *time.Time
		want      int
	}{
		{"5/week over 9 weeks", new(5), CadenceWeek, d(2026, 6, 1), d(2026, 7, 31), 45},
		{"12/month over 2 months", new(12), CadenceMonth, d(2026, 6, 1), d(2026, 7, 31), 24},
		{"nil count → 0", nil, CadenceMonth, d(2026, 6, 1), d(2026, 7, 31), 0},
		{"zero count → 0", new(0), CadenceMonth, d(2026, 6, 1), d(2026, 7, 31), 0},
		{"missing dates → count×1", new(10), CadenceMonth, nil, nil, 10},
	}
	for _, c := range cases {
		if got := EffectiveCount(c.perPeriod, c.cadence, c.start, c.end, time.UTC); got != c.want {
			t.Errorf("%s: EffectiveCount = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestWindowsWeekly(t *testing.T) {
	ws := Windows(CadenceWeek, d(2026, 6, 1), d(2026, 6, 20), time.UTC) // 20 days → 3 weeks
	if len(ws) != 3 {
		t.Fatalf("got %d windows, want 3", len(ws))
	}
	// First window starts at the campaign start; windows tile contiguously; the
	// last ends the day after the campaign end.
	if !ws[0].Start.Equal(*d(2026, 6, 1)) {
		t.Errorf("first window start = %v, want 2026-06-01", ws[0].Start)
	}
	if !ws[len(ws)-1].End.Equal(*d(2026, 6, 21)) {
		t.Errorf("last window end = %v, want 2026-06-21 (end+1)", ws[len(ws)-1].End)
	}
	for i := 1; i < len(ws); i++ {
		if !ws[i].Start.Equal(ws[i-1].End) {
			t.Errorf("window %d not contiguous: start %v, prev end %v", i, ws[i].Start, ws[i-1].End)
		}
	}
	if ws[0].Label != "Week 1" || ws[2].Label != "Week 3" {
		t.Errorf("labels = %q..%q, want Week 1..Week 3", ws[0].Label, ws[2].Label)
	}
}

func TestWindowsMonthly(t *testing.T) {
	// Mid-month start: the first window is clamped to the campaign start, the
	// second is a full calendar month clamped to the campaign end.
	ws := Windows(CadenceMonth, d(2026, 6, 15), d(2026, 7, 31), time.UTC)
	if len(ws) != 2 {
		t.Fatalf("got %d windows, want 2", len(ws))
	}
	if !ws[0].Start.Equal(*d(2026, 6, 15)) || !ws[0].End.Equal(*d(2026, 7, 1)) {
		t.Errorf("window 0 = [%v,%v), want [2026-06-15, 2026-07-01)", ws[0].Start, ws[0].End)
	}
	if !ws[1].Start.Equal(*d(2026, 7, 1)) || !ws[1].End.Equal(*d(2026, 8, 1)) {
		t.Errorf("window 1 = [%v,%v), want [2026-07-01, 2026-08-01)", ws[1].Start, ws[1].End)
	}
	if ws[0].Label != "Jun 2026" || ws[1].Label != "Jul 2026" {
		t.Errorf("labels = %q,%q, want Jun 2026,Jul 2026", ws[0].Label, ws[1].Label)
	}
}

func TestWindowsMissingDates(t *testing.T) {
	if ws := Windows(CadenceWeek, nil, d(2026, 7, 31), time.UTC); ws != nil {
		t.Errorf("expected nil windows for missing start, got %v", ws)
	}
}
