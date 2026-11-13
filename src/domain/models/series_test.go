package models

import (
	"testing"
	"time"
)

func TestSeriesRhythm_Valid(t *testing.T) {
	cases := []struct {
		name string
		r    SeriesRhythm
		want bool
	}{
		{"times per week", SeriesRhythm{Times: 2, Per: "week"}, true},
		{"times per month", SeriesRhythm{Times: 31, Per: "month"}, true},
		{"every 2 weeks", SeriesRhythm{Times: 1, Per: "week", Every: 2}, true},
		{"every 12 months", SeriesRhythm{Times: 1, Per: "month", Every: 12}, true},
		{"every Friday", SeriesRhythm{Times: 1, Per: "week", Weekday: "friday"}, true},
		{"every 2 weeks on Tuesday", SeriesRhythm{Times: 1, Per: "week", Every: 2, Weekday: "tuesday"}, true},
		{"zero times", SeriesRhythm{Times: 0, Per: "week"}, false},
		{"too many times", SeriesRhythm{Times: 32, Per: "month"}, false},
		{"bad unit", SeriesRhythm{Times: 1, Per: "day"}, false},
		{"every too long", SeriesRhythm{Times: 1, Per: "week", Every: 13}, false},
		{"negative every", SeriesRhythm{Times: 1, Per: "week", Every: -1}, false},
		{"unknown weekday", SeriesRhythm{Times: 1, Per: "week", Weekday: "fri"}, false},
		{"weekday twice a week", SeriesRhythm{Times: 2, Per: "week", Weekday: "friday"}, false},
		{"weekday per month", SeriesRhythm{Times: 1, Per: "month", Weekday: "friday"}, false},
	}
	for _, c := range cases {
		if got := c.r.Valid(); got != c.want {
			t.Errorf("%s: Valid(%+v) = %v, want %v", c.name, c.r, got, c.want)
		}
	}
}

func TestSeriesRhythm_Normalized(t *testing.T) {
	got := SeriesRhythm{Times: 1, Per: " Week ", Every: 1, Weekday: "Friday "}.Normalized()
	if want := (SeriesRhythm{Times: 1, Per: "week", Weekday: "friday"}); got != want {
		t.Fatalf("Normalized = %+v, want %+v", got, want)
	}
	if got.Interval() != 1 {
		t.Fatalf("Interval = %d, want 1", got.Interval())
	}
	if d, ok := got.Day(); !ok || d != time.Friday {
		t.Fatalf("Day = %v, %v", d, ok)
	}
	if _, ok := (SeriesRhythm{Times: 1, Per: "week"}).Day(); ok {
		t.Fatal("a rhythm with no weekday names a day")
	}
	if n := (SeriesRhythm{Times: 1, Per: "week", Every: 3}).Interval(); n != 3 {
		t.Fatalf("Interval = %d, want 3", n)
	}
}
