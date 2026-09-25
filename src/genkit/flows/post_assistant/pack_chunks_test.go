package post_assistant

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// packChunks carries each chunk's source citation to the assistant so it can
// name the slide/time range it drew from (CON-312); markdown chunks have none.
func TestPackChunks_CarriesSourceCitation(t *testing.T) {
	label := "0:45–1:30"
	anchor := &models.SourceAnchor{Kind: "time", StartMs: 45_000, EndMs: 90_000}
	out := packChunks([]models.AssetChunk{
		{ID: "a:0", ChunkIndex: 0, Content: "intro", TokenCount: 10, SourceLabel: &label, SourceAnchor: anchor},
		{ID: "b:0", ChunkIndex: 0, Content: "markdown", TokenCount: 10},
	})
	if len(out.Chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(out.Chunks))
	}
	if got := out.Chunks[0]; got.SourceLabel != label || got.SourceAnchor != anchor {
		t.Fatalf("citation not carried: %+v", got)
	}
	if got := out.Chunks[1]; got.SourceLabel != "" || got.SourceAnchor != nil {
		t.Fatalf("markdown chunk must have no citation: %+v", got)
	}
}
