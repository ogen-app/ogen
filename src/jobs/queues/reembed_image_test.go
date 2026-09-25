package queues

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TestReembedImage_KeepsRegionChunks: an edited description is re-embedded
// together with the stored region blocks, so the anchored region chunks survive
// the edit (CON-312).
func TestReembedImage_KeepsRegionChunks(t *testing.T) {
	d, assets, chunks, blocks, exts := baseImageDeps(&fakeImageClient{})
	assets.content = "a bar chart of Q3 revenue"
	exts.ext = &models.ImageExtraction{ID: "e1", AssetID: "a1", Status: models.ImageExtractionStatusComplete}
	blocks.got = []models.ImageBlock{
		{Text: "Revenue by month", Anchor: &models.SourceAnchor{Kind: "image", Bbox: &models.Bbox{X: 0.1, Y: 0.1, W: 0.5, H: 0.2}}},
	}

	p := &ReembedImageProcessor{Deps: d}
	if err := p.process(t.Context(), ReembedImageTask{AssetID: "a1", TenantID: "t1"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(chunks.got) != 2 {
		t.Fatalf("want description + region chunk, got %d", len(chunks.got))
	}
	if chunks.got[0].Content != "a bar chart of Q3 revenue" {
		t.Fatalf("description chunk = %q", chunks.got[0].Content)
	}
	if a := chunks.got[1].SourceAnchor; a == nil || a.Bbox == nil || a.Bbox.W != 0.5 {
		t.Fatalf("region anchor lost: %+v", a)
	}
	if len(assets.statuses) != 0 {
		t.Fatalf("re-embed must not touch the asset status, saw %v", assets.statuses)
	}
}

// TestReembedImage_ClearedDescriptionDropsStaleChunks: with no description and
// no text regions, the old chunks are removed rather than left stale.
func TestReembedImage_ClearedDescriptionDropsStaleChunks(t *testing.T) {
	d, _, chunks, _, exts := baseImageDeps(&fakeImageClient{})
	exts.ext = &models.ImageExtraction{ID: "e1", AssetID: "a1", Status: models.ImageExtractionStatusComplete}
	chunks.got = []models.AssetChunk{{Content: "stale"}}

	if err := (&ReembedImageProcessor{Deps: d}).process(t.Context(), ReembedImageTask{AssetID: "a1"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if chunks.calls != 1 || len(chunks.got) != 0 {
		t.Fatalf("want chunks cleared, calls=%d got=%d", chunks.calls, len(chunks.got))
	}
}

// TestReembedImage_SkipsUnsettledRun: a run still in flight owns the chunks and
// embeds on settle, so the re-embed leaves them alone.
func TestReembedImage_SkipsUnsettledRun(t *testing.T) {
	for _, st := range []string{models.ImageExtractionStatusDescribing, models.ImageExtractionStatusFailed} {
		d, assets, chunks, _, exts := baseImageDeps(&fakeImageClient{})
		assets.content = "edited"
		exts.ext = &models.ImageExtraction{ID: "e1", AssetID: "a1", Status: st}
		if err := (&ReembedImageProcessor{Deps: d}).process(t.Context(), ReembedImageTask{AssetID: "a1"}); err != nil {
			t.Fatalf("%s: process: %v", st, err)
		}
		if chunks.calls != 0 {
			t.Fatalf("%s: chunks must not be touched", st)
		}
	}
}

// TestReembedImage_NeverExtractedNoOp: no extraction yet → nothing to do.
func TestReembedImage_NeverExtractedNoOp(t *testing.T) {
	d, _, chunks, _, _ := baseImageDeps(&fakeImageClient{})
	if err := (&ReembedImageProcessor{Deps: d}).process(t.Context(), ReembedImageTask{AssetID: "a1"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if chunks.calls != 0 {
		t.Fatal("chunks must not be touched")
	}
}
