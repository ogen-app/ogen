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

func TestParseVariantMap(t *testing.T) {
	m, err := ParseVariantMap(`{"price-1":"111"," price-2 ":"222"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := m.Variant("price-2"); !ok || v != "222" {
		t.Fatalf("Variant(price-2) = %q, %v", v, ok)
	}
	if p, ok := m.Price("111"); !ok || p != "price-1" {
		t.Fatalf("Price(111) = %q, %v", p, ok)
	}
	if got := m.PriceIDs(); len(got) != 2 || got[0] != "price-1" {
		t.Fatalf("PriceIDs = %v", got)
	}
	if _, err := ParseVariantMap(`{"a":"1","b":"1"}`); err == nil {
		t.Fatal("duplicate variant accepted")
	}
	if _, err := ParseVariantMap(`not json`); err == nil {
		t.Fatal("bad json accepted")
	}
	if m, err := ParseVariantMap(""); err != nil || m.Len() != 0 {
		t.Fatalf("empty map: %v %d", err, m.Len())
	}
}

func TestParseSettings(t *testing.T) {
	s, err := ParseSettings("", true, "", "", false, "https://app/")
	if err != nil || s.Enabled() {
		t.Fatalf("default settings: %+v %v", s, err)
	}
	if _, err := ParseSettings("lemonsqueezy", true, "", "", false, ""); err == nil {
		t.Fatal("missing store id accepted")
	}
	if _, err := ParseSettings("stripe", true, "1", "", false, ""); err == nil {
		t.Fatal("unknown provider accepted")
	}
	s, err = ParseSettings("LemonSqueezy", false, "42", `{"p":"9"}`, true, "https://app/")
	if err != nil || !s.Enabled() || s.TestMode || s.AppBaseURL != "https://app" || s.Variants.Len() != 1 {
		t.Fatalf("lemonsqueezy settings: %+v %v", s, err)
	}
}

func TestVariantTrial(t *testing.T) {
	v := Variant{HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1}
	if !v.OneMonthTrial() || v.TrialDays() != 30 {
		t.Fatalf("one-month trial: %v %d", v.OneMonthTrial(), v.TrialDays())
	}
	v.TrialIntervalCount = 2
	if v.OneMonthTrial() {
		t.Fatal("two months reported as one")
	}
	if (Variant{}).TrialDays() != 0 {
		t.Fatal("no trial has days")
	}
}
