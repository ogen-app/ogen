package handlers

import (
	"strconv"
	"testing"
	"time"
)

// These are plain Go tests in the internal `handlers` package so they can reach
// the unexported limiter and swap its clock; the Ginkgo suite lives in the
// external handlers_test package.

func TestKeyedRateLimiter_AllowBurstThenBlock(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newKeyedRateLimiter(3, time.Hour)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("attempt %d: want allowed, got blocked", i+1)
		}
	}
	ok, retry := l.allow("k")
	if ok {
		t.Fatal("4th attempt: want blocked, got allowed")
	}
	if retry <= 0 {
		t.Fatalf("want a positive Retry-After, got %v", retry)
	}
}

func TestKeyedRateLimiter_PenalizeOnlyCharges_RetryAfterPeeksFree(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newKeyedRateLimiter(3, time.Hour)
	l.now = func() time.Time { return now }

	// A peek never consumes and never allocates a bucket for an unseen key.
	if d := l.retryAfter("k"); d != 0 {
		t.Fatalf("unseen key: want 0, got %v", d)
	}
	if len(l.buckets) != 0 {
		t.Fatalf("retryAfter allocated a bucket for an unseen key: %d buckets", len(l.buckets))
	}

	// Three failures are allowed (peek stays 0), the fourth is throttled.
	for i := range 3 {
		if d := l.retryAfter("k"); d != 0 {
			t.Fatalf("failure %d: want allowed (0), got %v", i+1, d)
		}
		l.penalize("k")
	}
	if d := l.retryAfter("k"); d <= 0 {
		t.Fatalf("after 3 penalties: want throttled, got %v", d)
	}
}

func TestKeyedRateLimiter_PenalizeClampsWaitAtWindowOverBurst(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	burst, window := 3.0, time.Hour
	l := newKeyedRateLimiter(burst, window)
	l.now = func() time.Time { return now }

	// Far more penalties than the burst must not drive the wait past the time for
	// a single token to refill — otherwise an attacker could lock a victim's key
	// for an unbounded stretch.
	for range 100 {
		l.penalize("k")
	}
	got := l.retryAfter("k")
	max := time.Duration(window.Seconds() / burst * float64(time.Second))
	if got <= 0 || got > max+time.Second {
		t.Fatalf("wait %v not bounded by window/burst (%v)", got, max)
	}
}

func TestKeyedRateLimiter_ResetRefundsKey(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newKeyedRateLimiter(3, time.Hour)
	l.now = func() time.Time { return now }

	for range 3 {
		l.penalize("k")
	}
	if l.retryAfter("k") <= 0 {
		t.Fatal("precondition: key should be throttled after 3 penalties")
	}
	l.reset("k")
	if d := l.retryAfter("k"); d != 0 {
		t.Fatalf("after reset: want 0, got %v", d)
	}
}

func TestKeyedRateLimiter_RefillsOverTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	burst, window := 3.0, time.Hour
	l := newKeyedRateLimiter(burst, window)
	l.now = func() time.Time { return now }

	for range 3 {
		l.penalize("k")
	}
	if l.retryAfter("k") <= 0 {
		t.Fatal("precondition: throttled after 3 penalties")
	}
	// One token refills after window/burst.
	now = now.Add(time.Duration(window.Seconds()/burst) * time.Second)
	if d := l.retryAfter("k"); d != 0 {
		t.Fatalf("after one refill interval: want 0, got %v", d)
	}
}

func TestKeyedRateLimiter_CapIsHardEvenWhenAllBucketsActive(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newKeyedRateLimiter(1, time.Hour)
	l.now = func() time.Time { return now }

	// Fill the map to the cap with active buckets — each penalize creates one at
	// lastSeen=now, so a sweep (ttl is 2×window) reclaims nothing.
	for i := range maxRateLimiterBuckets {
		l.penalize("k" + strconv.Itoa(i))
	}
	if len(l.buckets) != maxRateLimiterBuckets {
		t.Fatalf("setup: want %d buckets, got %d", maxRateLimiterBuckets, len(l.buckets))
	}

	// A brand-new key must not push the map past the cap when nothing can be swept.
	l.penalize("overflow")
	if len(l.buckets) > maxRateLimiterBuckets {
		t.Fatalf("penalize grew the map past the cap: %d", len(l.buckets))
	}
	// allow() on a new key at capacity fails open (permits) without tracking it.
	if ok, _ := l.allow("overflow-allow"); !ok {
		t.Fatal("allow on a new key at capacity should fail open (permit)")
	}
	if len(l.buckets) > maxRateLimiterBuckets {
		t.Fatalf("allow grew the map past the cap: %d", len(l.buckets))
	}

	// The cap didn't disable limiting: an already-tracked, exhausted key is still
	// blocked (its bucket was preserved, not evicted).
	if ok, _ := l.allow("k0"); ok {
		t.Fatal("an already-exhausted tracked key should still be blocked")
	}
}

func TestKeyedRateLimiter_KeysAreIndependent(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newKeyedRateLimiter(1, time.Hour)
	l.now = func() time.Time { return now }

	l.penalize("a") // exhaust "a"
	if l.retryAfter("a") <= 0 {
		t.Fatal(`"a" should be throttled`)
	}
	if d := l.retryAfter("b"); d != 0 {
		t.Fatalf(`"b" must be unaffected, got %v`, d)
	}
}
