package content_plan

import (
	"strings"
	"testing"
)

//go:fix inline

func TestJoinPagedChunks_NoPageInfo(t *testing.T) {
	got := joinPagedChunks([]chunkPart{
		{Content: "alpha"},
		{Content: "beta"},
	})
	want := "alpha\n\n[...]\n\nbeta"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestJoinPagedChunks_SinglePage(t *testing.T) {
	got := joinPagedChunks([]chunkPart{
		{Content: "first chunk", PageStart: new(2), PageEnd: new(2)},
	})
	if !strings.HasPrefix(got, "[p. 2] ") {
		t.Errorf("expected single-page prefix, got %q", got)
	}
}

func TestJoinPagedChunks_PageRange(t *testing.T) {
	got := joinPagedChunks([]chunkPart{
		{Content: "pages three through five", PageStart: new(3), PageEnd: new(5)},
	})
	if !strings.HasPrefix(got, "[pp. 3–5] ") {
		t.Errorf("expected page-range prefix, got %q", got)
	}
}

func TestJoinPagedChunks_MixedPagesAndPlain(t *testing.T) {
	parts := []chunkPart{
		{Content: "intro"},
		{Content: "middle", PageStart: new(4), PageEnd: new(4)},
		{Content: "outro", PageStart: new(10), PageEnd: new(12)},
	}
	got := joinPagedChunks(parts)
	if !strings.Contains(got, "[p. 4] middle") {
		t.Errorf("missing single-page citation: %q", got)
	}
	if !strings.Contains(got, "[pp. 10–12] outro") {
		t.Errorf("missing page-range citation: %q", got)
	}
	// First part has no page info — should appear as-is, without a prefix.
	if !strings.HasPrefix(got, "intro\n\n") {
		t.Errorf("plain first part should be unprefixed: %q", got)
	}
}

func TestJoinPagedChunks_Empty(t *testing.T) {
	if got := joinPagedChunks(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// The grounded-binding helpers dedupe by id, preserve order, and skip
// empty ids.
func TestAssetIDsOf(t *testing.T) {
	got := assetIDsOf([]resolvedPiece{
		{ID: "a", Title: "Alpha"},
		{ID: "b", Title: "Beta"},
		{ID: "a", Title: "Alpha dup"}, // deduped
		{ID: "", Title: "no id"},      // skipped
	})
	want := []string{"a", "b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if assetIDsOf(nil) != nil {
		t.Error("nil input should return nil")
	}
}

// A post's UsedAssetIDs is its self-reported assetRefs, filtered to the
// retrieved-context set — hallucinated ids dropped, dups removed, order kept, and
// an empty (non-nil) slice when nothing valid remains so the jsonb column stores
// [] rather than null.
func TestGroundedRefs(t *testing.T) {
	grounded := idSet([]string{"a", "b", "c"})

	got := groundedRefs([]string{"b", "x", "a", "b", ""}, grounded)
	want := []string{"b", "a"} // "x" not retrieved, second "b" deduped, "" skipped
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}

	// A post that cited nothing (or only hallucinated ids) gets a non-nil empty
	// slice, never nil.
	if got := groundedRefs(nil, grounded); got == nil || len(got) != 0 {
		t.Errorf("nil refs should yield non-nil empty slice, got %#v", got)
	}
	if got := groundedRefs([]string{"z"}, grounded); got == nil || len(got) != 0 {
		t.Errorf("unretrieved-only refs should yield non-nil empty slice, got %#v", got)
	}
}

func TestAssetRefsOf(t *testing.T) {
	got := assetRefsOf([]resolvedPiece{
		{ID: "a", Title: "Alpha"},
		{ID: "a", Title: "Alpha again"}, // deduped by id, first title kept
		{ID: "c", Title: "Gamma"},
		{ID: ""}, // skipped
	})
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(got), got)
	}
	if got[0] != (AssetRef{ID: "a", Title: "Alpha"}) || got[1] != (AssetRef{ID: "c", Title: "Gamma"}) {
		t.Fatalf("got %+v, want [{a Alpha} {c Gamma}]", got)
	}
	if assetRefsOf(nil) != nil {
		t.Error("nil input should return nil")
	}
}
