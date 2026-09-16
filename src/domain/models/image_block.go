package models

import (
	"time"

	"github.com/uptrace/bun"
)

// ImageBlock is one structured region extracted from a content-bank image
// (CON-281). It mirrors the documents-service Block shape (kind/level/text/
// cells/anchor) so the extraction output contract is identical across ingestion
// paths. Blocks are additive per run (keyed by extraction_id) and cascade-delete
// with the extraction and the asset. The embeddable text derived from these
// blocks (plus the description) lands separately in assets_chunks.
type ImageBlock struct {
	bun.BaseModel `bun:"table:image_blocks,alias:ib" swaggerignore:"true"`
	TenantScoped  // CON-97: tenant_id column + central scoping hooks

	ID           string `bun:"id,pk"                 json:"id"`
	ExtractionID string `bun:"extraction_id,notnull" json:"extraction_id"`
	AssetID      string `bun:"asset_id,notnull"      json:"asset_id"`
	Index        int    `bun:"index,notnull"         json:"index"`

	// Kind/Level/Text mirror a documents Block: kind is heading|paragraph|
	// list_item|table|composite|...; level is a heading depth (0 otherwise).
	Kind  string `bun:"kind,notnull"          json:"kind"`
	Level int    `bun:"level,notnull,default:0" json:"level"`
	Text  string `bun:"text,notnull,default:''" json:"text"`
	// Cells carries a table block's grid (row/col/text), stored as jsonb; nil for
	// non-table blocks.
	Cells []ImageCell `bun:"cells,type:jsonb" json:"cells,omitempty"`
	// Anchor is the image-region location (Kind == "image", a normalized bbox),
	// reusing the shared SourceAnchor jsonb shape.
	Anchor *SourceAnchor `bun:"anchor,type:jsonb" json:"anchor,omitempty"`
	// Provenance is "image_extraction"; LowConfidence marks a tabular block whose
	// source is a screenshot rather than a source file (CON-281 §9).
	Provenance    string `bun:"provenance,notnull,default:''"      json:"provenance,omitempty"`
	LowConfidence bool   `bun:"low_confidence,notnull,default:false" json:"low_confidence"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// ImageCell is one cell of a table ImageBlock's grid (CON-281).
type ImageCell struct {
	Row  int    `json:"row"`
	Col  int    `json:"col"`
	Text string `json:"text"`
}
