package queues

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestForEachTenantAlignsResultsAndRecoversPanics(t *testing.T) {
	boom := errors.New("tenant failed")
	items := []string{"ok", "fail", "panic", "ok-2"}
	results, errs := forEachTenant(t.Context(), items, func(ctx context.Context, item string) (int, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("%s: no per-tenant deadline", item)
		}
		switch item {
		case "fail":
			return 0, boom
		case "panic":
			panic("bad tenant data")
		}
		return len(item), nil
	})

	if results[0] != 2 || results[3] != 4 || errs[0] != nil || errs[3] != nil {
		t.Fatalf("results/errs not index-aligned: %v %v", results, errs)
	}
	if !errors.Is(errs[1], boom) {
		t.Errorf("errs[1] = %v, want %v", errs[1], boom)
	}
	if errs[2] == nil || !strings.Contains(errs[2].Error(), "bad tenant data") {
		t.Errorf("errs[2] = %v, want the recovered panic", errs[2])
	}
}

func TestStatusWriteContextOutlivesTheSweepButIsBounded(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, done := statusWriteContext(parent)
	defer done()
	if ctx.Err() != nil {
		t.Fatal("the status write must not inherit the sweep's cancellation")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("the status write must still have a deadline")
	}
}
