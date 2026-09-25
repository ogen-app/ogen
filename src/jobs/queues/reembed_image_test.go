package queues

import (
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
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

// TestReembedImage_WaitsForInFlightRun: a run still in flight may settle on
// pre-edit chunks, so the re-embed snoozes until it settles instead of dropping
// the edit (CON-312).
func TestReembedImage_WaitsForInFlightRun(t *testing.T) {
	d, assets, chunks, _, exts := baseImageDeps(&fakeImageClient{})
	assets.content = "edited"
	exts.ext = &models.ImageExtraction{ID: "e1", AssetID: "a1", Status: models.ImageExtractionStatusDescribing, UpdatedAt: time.Now()}
	err := (&ReembedImageProcessor{Deps: d}).process(t.Context(), ReembedImageTask{AssetID: "a1"})
	if _, ok := errors.AsType[*river.JobSnoozeError](err); !ok {
		t.Fatalf("want a snooze while the run is in flight, got %v", err)
	}
	if chunks.calls != 0 {
		t.Fatal("chunks must not be touched yet")
	}
}

// TestReembedImage_GivesUpOnStuckOrFailedRun: a failed run left no chunks, and a
// run idle past reembedStaleAfter is stuck — neither is waited on.
func TestReembedImage_GivesUpOnStuckOrFailedRun(t *testing.T) {
	for name, ext := range map[string]*models.ImageExtraction{
		"failed": {ID: "e1", Status: models.ImageExtractionStatusFailed, UpdatedAt: time.Now()},
		"stuck":  {ID: "e1", Status: models.ImageExtractionStatusExtracting, UpdatedAt: time.Now().Add(-3 * time.Hour)},
	} {
		d, _, chunks, _, exts := baseImageDeps(&fakeImageClient{})
		exts.ext = ext
		if err := (&ReembedImageProcessor{Deps: d}).process(t.Context(), ReembedImageTask{AssetID: "a1"}); err != nil {
			t.Fatalf("%s: process: %v", name, err)
		}
		if chunks.calls != 0 {
			t.Fatalf("%s: chunks must not be touched", name)
		}
	}
}

// TestProcessImage_KeepsDescriptionEditedMidRun: a description edited while the
// vision call runs is neither overwritten by the result nor left out of the
// chunks (CON-312).
func TestProcessImage_KeepsDescriptionEditedMidRun(t *testing.T) {
	client := &fakeImageClient{res: &imageclient.ExtractResult{
		Description: "generated description", DescriptionOK: true, ExtractionOK: true,
	}}
	d, assets, chunks, _, _ := baseImageDeps(client)
	client.during = func() { assets.content = "the user's own description" }

	task := ProcessImageTask{AssetID: "a1", RunKey: "run-1", StorageKey: "assets/a1/original.png", MimeType: "image/png"}
	if err := newImageProc(d).process(t.Context(), task, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if assets.content != "the user's own description" {
		t.Fatalf("edit overwritten: content = %q", assets.content)
	}
	if len(chunks.got) != 1 || chunks.got[0].Content != "the user's own description" {
		t.Fatalf("chunks don't embed the edit: %+v", chunks.got)
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
