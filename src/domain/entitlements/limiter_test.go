package entitlements

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// fakeResolver returns a fixed Resolution (or error) for ResolveCurrent.
type fakeResolver struct {
	res *Resolution
	err error
}

func (f fakeResolver) ResolveCurrent(context.Context, string) (*Resolution, error) {
	return f.res, f.err
}

func resWith(vals map[string]any) *Resolution {
	r := &Resolution{}
	for k, v := range vals {
		r.Entitlements = append(r.Entitlements, EntitlementValue{Feature: Feature{Key: k}, Value: v})
	}
	return r
}

func fixedCount(n int64) Counter {
	return CounterFunc(func(context.Context, string) (int64, error) { return n, nil })
}

func TestLimiterAllow(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		mode    Mode
		limit   any   // float64 or nil (unlimited) or absent
		present bool  // whether team_seats is on the version
		current int64 // registered counter's value
		want    bool  // Allowed
	}{
		{"under cap", ModeEnforce, float64(3), true, 2, true},
		{"at cap", ModeEnforce, float64(3), true, 3, false},
		{"over cap", ModeEnforce, float64(3), true, 5, false},
		{"unlimited", ModeEnforce, nil, true, 999, true},
		{"zero cap denies", ModeEnforce, float64(0), true, 0, false},
		{"absent feature", ModeEnforce, nil, false, 999, true},
		{"warn never blocks", ModeWarn, float64(1), true, 9, true},
		{"off never blocks", ModeOff, float64(1), true, 9, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals := map[string]any{}
			if tc.present {
				vals["team_seats"] = tc.limit
			}
			l := NewLimiter(fakeResolver{res: resWith(vals)}, nil, tc.mode)
			l.Register("team_seats", fixedCount(tc.current))
			dec, err := l.Allow(ctx, "tn-1", "team_seats")
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if dec.Allowed != tc.want {
				t.Fatalf("Allowed = %v, want %v (%+v)", dec.Allowed, tc.want, dec)
			}
			if reqErr := l.Require(ctx, "tn-1", "team_seats"); (reqErr != nil) == tc.want {
				t.Fatalf("Require returned err=%v but Allowed=%v", reqErr, tc.want)
			}
		})
	}
}

func TestLimiterAllowFailsOpen(t *testing.T) {
	l := NewLimiter(fakeResolver{err: errors.New("db down")}, nil, ModeEnforce)
	l.Register("team_seats", fixedCount(999))
	dec, err := l.Allow(context.Background(), "tn-1", "team_seats")
	if err != nil || !dec.Allowed {
		t.Fatalf("resolve failure must fail open: dec=%+v err=%v", dec, err)
	}
}

func TestLimiterGate(t *testing.T) {
	ctx := context.Background()
	on := NewLimiter(fakeResolver{res: resWith(map[string]any{"custom_campaign_types": true})}, nil, ModeEnforce)
	off := NewLimiter(fakeResolver{res: resWith(map[string]any{"custom_campaign_types": false})}, nil, ModeEnforce)
	warn := NewLimiter(fakeResolver{res: resWith(map[string]any{"custom_campaign_types": false})}, nil, ModeWarn)

	if ok, _ := on.Gate(ctx, "tn", "custom_campaign_types"); !ok {
		t.Fatal("gate on should allow")
	}
	if ok, _ := off.Gate(ctx, "tn", "custom_campaign_types"); ok {
		t.Fatal("gate off (enforce) should deny")
	}
	if err := off.RequireGate(ctx, "tn", "custom_campaign_types"); err == nil {
		t.Fatal("RequireGate off (enforce) should error")
	}
	if ok, _ := warn.Gate(ctx, "tn", "custom_campaign_types"); !ok {
		t.Fatal("gate off (warn) should allow")
	}
	// A nil limiter is a permissive no-op.
	var nilL *Limiter
	if err := nilL.Require(ctx, "tn", "team_seats"); err != nil {
		t.Fatalf("nil limiter Require: %v", err)
	}
	if err := nilL.RequireGate(ctx, "tn", "x"); err != nil {
		t.Fatalf("nil limiter RequireGate: %v", err)
	}
}

func TestLimiterMultiplier(t *testing.T) {
	ctx := context.Background()
	l := NewLimiter(fakeResolver{res: resWith(map[string]any{"assistant_multiplier": float64(20)})}, nil, ModeEnforce)
	if got := l.Multiplier(ctx, "tn", "assistant_multiplier"); got != 20 {
		t.Fatalf("multiplier = %v, want 20", got)
	}
	// Absent → default 1.
	l2 := NewLimiter(fakeResolver{res: resWith(map[string]any{})}, nil, ModeEnforce)
	if got := l2.Multiplier(ctx, "tn", "assistant_multiplier"); got != 1 {
		t.Fatalf("absent multiplier = %v, want 1", got)
	}
}

// spyNotifier records the crossings the Limiter reports.
type spyNotifier struct{ events []LimitEvent }

func (s *spyNotifier) NotifyLimit(_ context.Context, _ string, ev LimitEvent) {
	s.events = append(s.events, ev)
}

func TestLimiterNearLimitNotify(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		mode    Mode
		limit   float64
		current int64
		want    []LimitState
	}{
		{"below band is silent", ModeEnforce, 10, 7, nil},
		{"crossing the band warns once", ModeEnforce, 10, 8, []LimitState{LimitApproaching}},
		{"filling the cap reports reached", ModeEnforce, 10, 9, []LimitState{LimitReached}},
		{"a denied create is silent", ModeEnforce, 10, 10, nil},
		{"warn mode still notifies", ModeWarn, 10, 8, []LimitState{LimitApproaching}},
		{"warn mode over cap is silent", ModeWarn, 10, 12, nil},
		{"off mode is silent", ModeOff, 10, 8, nil},
		{"tiny cap only reports reached", ModeEnforce, 1, 0, []LimitState{LimitReached}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyNotifier{}
			l := NewLimiter(fakeResolver{res: resWith(map[string]any{"team_seats": tc.limit})}, nil, tc.mode).
				WithNotifier(spy, 90)
			l.Register("team_seats", fixedCount(tc.current))
			if _, err := l.Allow(ctx, "tn-1", "team_seats"); err != nil {
				t.Fatalf("Allow: %v", err)
			}
			got := make([]LimitState, len(spy.events))
			for i, e := range spy.events {
				got[i] = e.State
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("states = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLimiterNearLimitEventShape(t *testing.T) {
	spy := &spyNotifier{}
	l := NewLimiter(fakeResolver{res: resWith(map[string]any{"team_seats": float64(10)})}, nil, ModeEnforce).
		WithNotifier(spy, 90)
	l.Register("team_seats", fixedCount(8)) // next = 9 = ceil(10*90%) → approaching
	if _, err := l.Allow(context.Background(), "tn-1", "team_seats"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if len(spy.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(spy.events))
	}
	ev := spy.events[0]
	if ev.Feature != "team_seats" || ev.State != LimitApproaching || ev.Limit != 10 || ev.Current != 9 || ev.Percent != 90 {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestLimiterWithNotifierClampsThreshold(t *testing.T) {
	for in, want := range map[int]int{0: 90, -5: 90, 101: 90, 1: 1, 50: 50, 100: 100} {
		l := NewLimiter(fakeResolver{res: resWith(nil)}, nil, ModeEnforce).WithNotifier(&spyNotifier{}, in)
		if l.warnPct != want {
			t.Fatalf("WithNotifier(%d) → warnPct %d, want %d", in, l.warnPct, want)
		}
	}
	// A nil notifier disables near-limit notifications without firing.
	spy := &spyNotifier{}
	l := NewLimiter(fakeResolver{res: resWith(map[string]any{"team_seats": float64(2)})}, nil, ModeEnforce)
	l.Register("team_seats", fixedCount(1)) // would be "reached" if a notifier were set
	if _, err := l.Allow(context.Background(), "tn", "team_seats"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if len(spy.events) != 0 {
		t.Fatalf("no-notifier limiter should not fire, got %d", len(spy.events))
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"enforce": ModeEnforce, "off": ModeOff, "warn": ModeWarn, "": ModeWarn, "bogus": ModeWarn} {
		if got := ParseMode(in); got != want {
			t.Fatalf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}
