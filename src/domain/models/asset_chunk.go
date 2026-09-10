package models

import (
	"encoding/json"
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"
)

// AssetChunk stores one chunk of an Asset's text content together with its
// embedding vector. A short asset produces a single chunk (chunk_index = 0).
// Longer assets are split into multiple overlapping chunks.
type AssetChunk struct {
	bun.BaseModel `bun:"table:assets_chunks,alias:ac" swaggerignore:"true"`
	TenantScoped  // CON-97: tenant_id column + central scoping hooks

	ID         string `bun:"id,pk"                                        json:"id"`
	AssetID    string `bun:"asset_id,notnull"                             json:"asset_id"`
	ChunkIndex int    `bun:"chunk_index,notnull"                          json:"chunk_index"`
	PageStart  *int   `bun:"page_start"                                   json:"page_start"`
	PageEnd    *int   `bun:"page_end"                                     json:"page_end"`
	// SourceLabel is a human-readable citation for the chunk's origin (CON-280),
	// e.g. "Slide 4", "Sheet 'Q3 Pipeline' rows 10-24", or a heading breadcrumb.
	// nil for chunks that predate document ingestion (PDF/URL/MD).
	SourceLabel *string `bun:"source_label"                              json:"source_label,omitempty"`
	// SourceAnchor is the structured location backing SourceLabel (CON-280),
	// stored as jsonb. nil for non-document chunks.
	SourceAnchor *SourceAnchor `bun:"source_anchor,type:jsonb"          json:"source_anchor,omitempty"`
	Content      string        `bun:"content,notnull"                    json:"content"`
	TokenCount   int           `bun:"token_count,notnull"                json:"token_count"`
	// Embedding is the chunk's 3072-dim Gemini Embedding 2 vector (CON-101),
	// stored in a pgvector halfvec(3072) column. halfvec (16-bit floats) is
	// required because pgvector's full-precision vector HNSW index caps at 2000
	// dimensions. Similarity search runs in-database via the `<=>`
	// cosine-distance operator (see AssetChunksRepository.SearchSimilar).
	Embedding pgvector.HalfVector `bun:"embedding,type:halfvec(3072)" json:"-"`
	Model     string              `bun:"model"                     json:"model"`
	CreatedAt time.Time           `bun:"created_at,notnull,default:current_timestamp" json:"-"`
}

// SourceAnchor is the structured source location of a document chunk (CON-280),
// persisted as jsonb in assets_chunks.source_anchor. Which fields are populated
// depends on Kind: page for prose flow, slide for decks, sheet+CellRange for
// spreadsheets, HeadingPath for structured prose, headers-as-metadata for
// email, and StartMs/EndMs for audio transcript time-ranges (CON-282).
// Zero-valued fields are omitted from the stored JSON.
type SourceAnchor struct {
	Kind        string   `json:"kind"` // page|slide|sheet|section|email|time
	Page        int      `json:"page,omitempty"`
	Slide       int      `json:"slide,omitempty"`
	Sheet       string   `json:"sheet,omitempty"`
	CellRange   string   `json:"cell_range,omitempty"`
	HeadingPath []string `json:"heading_path,omitempty"`
	// StartMs/EndMs bound an audio transcript chunk on the ORIGINAL asset
	// timeline (CON-282), Kind == "time". Provenance marks how the anchor was
	// derived (e.g. "transcript"). All three are empty for non-audio anchors.
	StartMs    int64  `json:"start_ms,omitempty"`
	EndMs      int64  `json:"end_ms,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// MarshalJSON keeps start_ms/end_ms present for time anchors even at 0 ms: the
// first audio chunk legitimately starts at 0, and the struct's `omitempty` tag
// would drop it, leaving consumers unable to tell "0" from "absent" (CON-282).
// Non-time anchors keep their omitempty semantics, so page/slide/sheet chunks
// never gain empty time fields. The embedded alias avoids infinite recursion;
// the shallower explicit fields shadow its omitempty ones for time anchors.
func (a SourceAnchor) MarshalJSON() ([]byte, error) {
	type alias SourceAnchor
	if a.Kind == "time" {
		return json.Marshal(struct {
			alias
			StartMs int64 `json:"start_ms"`
			EndMs   int64 `json:"end_ms"`
		}{alias: alias(a), StartMs: a.StartMs, EndMs: a.EndMs})
	}
	return json.Marshal(alias(a))
}
