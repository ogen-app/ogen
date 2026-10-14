package models

import (
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"
)

// AssetChunkStage is one chunk of an ingest run still embedding. The run's
// embedded stages become the asset's chunks when the run commits.
type AssetChunkStage struct {
	bun.BaseModel `bun:"table:asset_chunk_stages,alias:acs" swaggerignore:"true"`
	TenantScoped

	RunID        string               `bun:"run_id,pk"`
	ChunkIndex   int                  `bun:"chunk_index,pk"`
	AssetID      string               `bun:"asset_id,notnull"`
	Content      string               `bun:"content,notnull"`
	TokenCount   int                  `bun:"token_count,notnull"`
	PageStart    *int                 `bun:"page_start"`
	PageEnd      *int                 `bun:"page_end"`
	SourceLabel  *string              `bun:"source_label"`
	SourceAnchor *SourceAnchor        `bun:"source_anchor,type:jsonb"`
	Embedding    *pgvector.HalfVector `bun:"embedding,type:halfvec(3072)"`
	Model        string               `bun:"model,nullzero"`
	Failed       bool                 `bun:"failed,notnull"`
	CreatedAt    time.Time            `bun:"created_at,notnull,default:current_timestamp"`
}
