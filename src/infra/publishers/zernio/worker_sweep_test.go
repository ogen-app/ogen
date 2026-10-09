package zernio

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestIntegrationFastTenantsArePerTenant(t *testing.T) {
	integ := NewIntegration(nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	integ.BumpFastUntil("t-a", now.Add(time.Minute))
	integ.BumpFastUntil("t-b", now.Add(-time.Second))
	integ.BumpFastUntil("t-a", now.Add(time.Second)) // an earlier deadline never shortens the window

	if got := integ.FastTenants(now); !slices.Equal(got, []string{"t-a"}) {
		t.Fatalf("fast tenants = %v, want [t-a]", got)
	}
	if got := integ.FastTenants(now.Add(30 * time.Second)); !slices.Equal(got, []string{"t-a"}) {
		t.Fatalf("fast tenants at +30s = %v, want [t-a] (window runs to +1m)", got)
	}
	if got := integ.FastTenants(now.Add(2 * time.Minute)); len(got) != 0 {
		t.Fatalf("fast tenants after the window = %v, want none", got)
	}
}

func TestWorkerSweepSkipsWithoutLock(t *testing.T) {
	listed := 0
	w := NewWorker(NewIntegration(nil), nil, nil, nil, nil, nil, time.Minute, time.Second,
		func(context.Context) ([]string, error) {
			listed++
			return nil, nil
		})

	w.SetSweepLock(func(context.Context) (func(), bool) { return nil, false })
	if err := w.SyncOnce(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if listed != 0 {
		t.Fatalf("a sweep without the lock listed tenants %d times, want 0", listed)
	}

	released := false
	w.SetSweepLock(func(context.Context) (func(), bool) { return func() { released = true }, true })
	if err := w.SyncOnce(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if listed != 1 || !released {
		t.Fatalf("a sweep holding the lock: listed %d, released %v; want 1, true", listed, released)
	}
}
