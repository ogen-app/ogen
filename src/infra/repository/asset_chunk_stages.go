package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// chunkStageTTL is how long a crashed run's stages are kept before a later
// run sweeps them.
const chunkStageTTL = 48 * time.Hour

// ChunkStageStats tallies a run when it commits: Attempts staged texts,
// Embedded those with a vector, Failures those whose embed failed, Tokens the
// embedded ones' token counts.
type ChunkStageStats struct {
	Attempts int
	Embedded int
	Failures int
	Tokens   int64
}

// AssetChunkStagesRepository stages an ingest run's chunks while it embeds.
type AssetChunkStagesRepository interface {
	// Begin stages a run's chunks, dropping the asset's earlier runs and
	// stale runs of other assets.
	Begin(ctx context.Context, runID, assetID string, stages []models.AssetChunkStage) error
	// Pending returns up to limit of the run's stages still to embed, in
	// chunk order.
	Pending(ctx context.Context, runID string, limit int) ([]models.AssetChunkStage, error)
	// Rows returns the run's stages at the given chunk indexes, in chunk
	// order.
	Rows(ctx context.Context, runID string, idx []int) ([]models.AssetChunkStage, error)
	// SetEmbeddings stores vectors by chunk index.
	SetEmbeddings(ctx context.Context, runID, model string, vecs map[int][]float32) error
	// MarkFailed marks chunks whose embed failed.
	MarkFailed(ctx context.Context, runID string, idx []int) error
	// Commit replaces the asset's chunks with the run's embedded stages and
	// drops the run. Nothing is replaced when the run embedded nothing,
	// unless clearStale and nothing was staged, which clears chunks left over
	// from earlier content.
	Commit(ctx context.Context, runID, assetID string, clearStale bool) (ChunkStageStats, error)
}

type assetChunkStagesRepository struct {
	db *bun.DB
}

func NewAssetChunkStagesRepository(db *bun.DB) AssetChunkStagesRepository {
	return &assetChunkStagesRepository{db: db}
}

func (r *assetChunkStagesRepository) Begin(ctx context.Context, runID, assetID string, stages []models.AssetChunkStage) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.AssetChunkStage)(nil)).
			Where("asset_id = ? OR created_at < ?", assetID, time.Now().Add(-chunkStageTTL)).
			Exec(ctx); err != nil {
			return fmt.Errorf("drop earlier stages: %w", err)
		}
		if len(stages) == 0 {
			return nil
		}
		for i := range stages {
			stages[i].RunID, stages[i].AssetID = runID, assetID
		}
		if _, err := tx.NewInsert().Model(&stages).Exec(ctx); err != nil {
			return fmt.Errorf("stage chunks: %w", err)
		}
		return nil
	})
}

func (r *assetChunkStagesRepository) Pending(ctx context.Context, runID string, limit int) ([]models.AssetChunkStage, error) {
	var out []models.AssetChunkStage
	err := r.db.NewSelect().Model(&out).
		ExcludeColumn("embedding").
		Where("acs.run_id = ?", runID).
		Where("acs.embedding IS NULL").
		Where("NOT acs.failed").
		OrderExpr("acs.chunk_index ASC").
		Limit(limit).
		Scan(ctx)
	return out, err
}

func (r *assetChunkStagesRepository) Rows(ctx context.Context, runID string, idx []int) ([]models.AssetChunkStage, error) {
	var out []models.AssetChunkStage
	if len(idx) == 0 {
		return out, nil
	}
	err := r.db.NewSelect().Model(&out).
		ExcludeColumn("embedding").
		Where("acs.run_id = ?", runID).
		Where("acs.chunk_index IN (?)", bun.List(idx)).
		OrderExpr("acs.chunk_index ASC").
		Scan(ctx)
	return out, err
}

func (r *assetChunkStagesRepository) SetEmbeddings(ctx context.Context, runID, model string, vecs map[int][]float32) error {
	if len(vecs) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for idx, v := range vecs {
			hv := pgvector.NewHalfVector(v)
			if _, err := tx.NewUpdate().Model((*models.AssetChunkStage)(nil)).
				Set("embedding = ?", hv).
				Set("model = ?", model).
				Where("run_id = ? AND chunk_index = ?", runID, idx).
				Exec(ctx); err != nil {
				return fmt.Errorf("store embedding %d: %w", idx, err)
			}
		}
		return nil
	})
}

func (r *assetChunkStagesRepository) MarkFailed(ctx context.Context, runID string, idx []int) error {
	if len(idx) == 0 {
		return nil
	}
	_, err := r.db.NewUpdate().Model((*models.AssetChunkStage)(nil)).
		Set("failed = true").
		Where("run_id = ?", runID).
		Where("chunk_index IN (?)", bun.List(idx)).
		Exec(ctx)
	return err
}

func (r *assetChunkStagesRepository) Commit(ctx context.Context, runID, assetID string, clearStale bool) (ChunkStageStats, error) {
	var st ChunkStageStats
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().TableExpr("asset_chunk_stages").
			ColumnExpr("count(*) AS attempts").
			ColumnExpr("count(embedding) AS embedded").
			ColumnExpr("count(*) FILTER (WHERE failed) AS failures").
			ColumnExpr("COALESCE(sum(token_count) FILTER (WHERE embedding IS NOT NULL), 0) AS tokens").
			Where("run_id = ?", runID).
			Scan(ctx, &st.Attempts, &st.Embedded, &st.Failures, &st.Tokens); err != nil {
			return fmt.Errorf("tally stages: %w", err)
		}
		if st.Embedded > 0 || (clearStale && st.Attempts == 0) {
			if _, err := tx.NewDelete().Model((*models.AssetChunk)(nil)).
				Where("asset_id = ?", assetID).
				Exec(ctx); err != nil {
				return fmt.Errorf("delete existing chunks: %w", err)
			}
			if _, err := tx.NewRaw(`
				INSERT INTO assets_chunks (id, tenant_id, asset_id, chunk_index, page_start, page_end,
					source_label, source_anchor, content, token_count, embedding, model)
				SELECT asset_id || ':' || chunk_index, tenant_id, asset_id, chunk_index, page_start, page_end,
					source_label, source_anchor, content, token_count, embedding, model
				FROM asset_chunk_stages
				WHERE run_id = ? AND embedding IS NOT NULL`, runID).Exec(ctx); err != nil {
				return fmt.Errorf("insert chunks: %w", err)
			}
		}
		if _, err := tx.NewDelete().Model((*models.AssetChunkStage)(nil)).
			Where("run_id = ?", runID).
			Exec(ctx); err != nil {
			return fmt.Errorf("drop run: %w", err)
		}
		return nil
	})
	return st, err
}
