package tenantctx_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func TestWithFromRoundTrip(t *testing.T) {
	ctx := tenantctx.With(t.Context(), "tn-1")
	id, ok := tenantctx.From(ctx)
	if !ok || id != "tn-1" {
		t.Fatalf("expected tn-1/true, got %q/%v", id, ok)
	}
}

func TestFromAbsent(t *testing.T) {
	if id, ok := tenantctx.From(t.Context()); ok || id != "" {
		t.Fatalf("expected empty/false, got %q/%v", id, ok)
	}
}

// An empty tenant id must read back as fail-closed (not present), so the
// scoped query layer never runs an unscoped query (CON-97 §6).
func TestFromEmptyStringIsAbsent(t *testing.T) {
	ctx := tenantctx.With(t.Context(), "")
	if id, ok := tenantctx.From(ctx); ok || id != "" {
		t.Fatalf("expected empty/false for empty tenant, got %q/%v", id, ok)
	}
}
