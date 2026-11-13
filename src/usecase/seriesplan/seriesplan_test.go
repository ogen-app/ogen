package seriesplan

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func TestDays(t *testing.T) {
	// Thursday 2026-10-01 to Wednesday 2026-10-28: four whole weeks.
	start, end := new(day(time.October, 1)), new(day(time.October, 28))
	cases := []struct {
		name string
		r    *models.SeriesRhythm
		want []time.Time
	}{
		{"occasional", nil, nil},
		{"invalid", &models.SeriesRhythm{Times: 0, Per: "week"}, nil},
		{"twice a week", &models.SeriesRhythm{Times: 2, Per: "week"}, []time.Time{
			day(time.October, 1), day(time.October, 4), day(time.October, 8), day(time.October, 11),
			day(time.October, 15), day(time.October, 18), day(time.October, 22), day(time.October, 25),
		}},
		{"every Friday", &models.SeriesRhythm{Times: 1, Per: "week", Weekday: "friday"}, []time.Time{
			day(time.October, 2), day(time.October, 9), day(time.October, 16), day(time.October, 23),
		}},
		{"every 2 weeks on Tuesday", &models.SeriesRhythm{Times: 1, Per: "week", Every: 2, Weekday: "tuesday"}, []time.Time{
			day(time.October, 6), day(time.October, 20),
		}},
		{"every 2 weeks", &models.SeriesRhythm{Times: 1, Per: "week", Every: 2}, []time.Time{
			day(time.October, 1), day(time.October, 15),
		}},
		{"once a month", &models.SeriesRhythm{Times: 1, Per: "month"}, []time.Time{day(time.October, 1)}},
	}
	for _, c := range cases {
		got := Days(c.r, start, end, time.UTC)
		if len(got) != len(c.want) {
			t.Errorf("%s: Days = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if !got[i].Equal(c.want[i]) {
				t.Errorf("%s: Days = %v, want %v", c.name, got, c.want)
				break
			}
		}
		if n := Claim(c.r, start, end, time.UTC); n != len(c.want) {
			t.Errorf("%s: Claim = %d, want %d", c.name, n, len(c.want))
		}
	}
}

func TestDays_PartialTrailingWeek(t *testing.T) {
	// Thursday to the next Sunday: one whole week and a Thursday-Sunday tail.
	start, end := new(day(time.October, 1)), new(day(time.October, 11))
	// The tail has a Friday, so it gets one; it has no Tuesday, so it gets none.
	if got := Claim(&models.SeriesRhythm{Times: 1, Per: "week", Weekday: "friday"}, start, end, time.UTC); got != 2 {
		t.Fatalf("Friday claim = %d, want 2", got)
	}
	if got := Claim(&models.SeriesRhythm{Times: 1, Per: "week", Weekday: "tuesday"}, start, end, time.UTC); got != 1 {
		t.Fatalf("Tuesday claim = %d, want 1", got)
	}
	// Without a weekday the claim is times per started period, as the goal counts it.
	if got := Claim(&models.SeriesRhythm{Times: 2, Per: "week"}, start, end, time.UTC); got != 4 {
		t.Fatalf("twice-a-week claim = %d, want 4", got)
	}
}

func TestDays_CampaignTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	// 2026-10-02 03:00 UTC is still Thursday 1 October in Los Angeles, and
	// 2026-10-08 03:00 UTC still Wednesday 7 October: one week there.
	start := new(time.Date(2026, time.October, 2, 3, 0, 0, 0, time.UTC))
	end := new(time.Date(2026, time.October, 8, 3, 0, 0, 0, time.UTC))
	got := Days(&models.SeriesRhythm{Times: 1, Per: "week", Weekday: "thursday"}, start, end, loc)
	if len(got) != 1 || got[0].Day() != 1 || got[0].Location() != loc {
		t.Fatalf("Days = %v", got)
	}
}
