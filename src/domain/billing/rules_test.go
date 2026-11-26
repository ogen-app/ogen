package billing

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestLive(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)
	cases := []struct {
		status string
		endsAt *time.Time
		want   bool
	}{
		{models.SubscriptionStatusOnTrial, nil, true},
		{models.SubscriptionStatusActive, nil, true},
		{models.SubscriptionStatusPastDue, nil, true},
		{models.SubscriptionStatusUnpaid, nil, true},
		{models.SubscriptionStatusPaused, nil, true},
		{models.SubscriptionStatusCancelled, &later, true},
		{models.SubscriptionStatusCancelled, &earlier, false},
		{models.SubscriptionStatusCancelled, nil, false},
		{models.SubscriptionStatusExpired, nil, false},
		{"something_new", nil, false},
	}
	for _, c := range cases {
		if got := Live(c.status, c.endsAt, now); got != c.want {
			t.Errorf("Live(%s, %v) = %v, want %v", c.status, c.endsAt, got, c.want)
		}
	}
}

func TestDirection(t *testing.T) {
	proM := Price{ID: "pro-m", TierName: "Pro", Interval: "month", NetMinor: 2900}
	proY := Price{ID: "pro-y", TierName: "Pro", Interval: "year", NetMinor: 29000}
	maxM := Price{ID: "max-m", TierName: "Max", Interval: "month", NetMinor: 7900}
	cases := []struct {
		from, to Price
		want     string
	}{
		{proM, maxM, DirectionUpgrade},
		{maxM, proM, DirectionDowngrade},
		{proM, proM, DirectionSame},
		// 29000 a year is less than 12 x 2900 = 34800, so yearly is cheaper.
		{proM, proY, DirectionDowngrade},
		{proY, proM, DirectionUpgrade},
		// A yearly price equal to twelve months on the same tier is still a
		// change: yearly ranks above monthly.
		{proM, Price{ID: "pro-y-even", TierName: "Pro", Interval: "year", NetMinor: 34800}, DirectionUpgrade},
		{Price{ID: "pro-y-even", TierName: "Pro", Interval: "year", NetMinor: 34800}, proM, DirectionDowngrade},
		// Equal amounts fall back to the tier name.
		{Price{ID: "a", TierName: "Alpha", Interval: "month", NetMinor: 100}, Price{ID: "b", TierName: "Beta", Interval: "month", NetMinor: 100}, DirectionUpgrade},
	}
	for _, c := range cases {
		if got := Direction(c.from, c.to); got != c.want {
			t.Errorf("Direction(%s → %s) = %s, want %s", c.from.ID, c.to.ID, got, c.want)
		}
	}
}
