package handlers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTTLCache(t *testing.T) {
	ctx := t.Context()
	c := newTTLCache[int](time.Minute, 2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	var loads atomic.Int32
	load := func(v int) func(context.Context) (int, error) {
		return func(context.Context) (int, error) {
			loads.Add(1)
			return v, nil
		}
	}

	if v, _ := c.get(ctx, "a", load(1)); v != 1 {
		t.Fatalf("first get = %d, want 1", v)
	}
	if v, _ := c.get(ctx, "a", load(2)); v != 1 || loads.Load() != 1 {
		t.Fatalf("cached get = %d after %d loads, want 1 after 1", v, loads.Load())
	}

	now = now.Add(2 * time.Minute)
	if v, _ := c.get(ctx, "a", load(3)); v != 3 {
		t.Fatalf("get after expiry = %d, want a fresh 3", v)
	}

	boom := errors.New("upstream down")
	if _, err := c.get(ctx, "b", func(context.Context) (int, error) { return 0, boom }); !errors.Is(err, boom) {
		t.Fatalf("failed load err = %v, want %v", err, boom)
	}
	if v, err := c.get(ctx, "b", load(4)); err != nil || v != 4 {
		t.Fatalf("get after a failed load = (%d, %v), want a retried 4", v, err)
	}

	// Concurrent misses for one key share a single load.
	release := make(chan struct{})
	var shared atomic.Int32
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			_, _ = c.get(ctx, "c", func(context.Context) (int, error) {
				shared.Add(1)
				<-release
				return 5, nil
			})
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if shared.Load() != 1 {
		t.Fatalf("concurrent misses ran %d loads, want 1", shared.Load())
	}

	if len(c.entries) > 2 {
		t.Fatalf("cache holds %d entries, want at most 2", len(c.entries))
	}
}
