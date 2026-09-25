package repository

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// AssetChunksRepository handles persistence of asset text chunks and their
// embedding vectors.
type AssetChunksRepository interface {
	// UpsertChunks atomically replaces all chunks for the given asset.
	UpsertChunks(ctx context.Context, assetID string, chunks []models.AssetChunk) error
	// GetByAssetID returns all chunks for an asset, ordered by chunk_index.
	GetByAssetID(ctx context.Context, assetID string) ([]models.AssetChunk, error)
	// ListPageByAssetID returns one page of an asset's chunks ordered by
	// chunk_index, without the embedding vector, plus the asset's total chunk
	// count — the REST chunk view (CON-312).
	ListPageByAssetID(ctx context.Context, assetID string, offset, limit int) ([]models.AssetChunk, int, error)
	// SearchSimilar returns embedded chunks ordered by cosine similarity to
	// query (closest first), keeping only those scoring >= minScore. When
	// assetIDs is non-empty the search is scoped to those assets; limit <= 0
	// means no row cap. Backed by the pgvector HNSW index (embedding <=> query).
	SearchSimilar(ctx context.Context, query pgvector.HalfVector, assetIDs []string, minScore float64, limit int) ([]models.AssetChunk, error)
	// DeleteByAssetID removes all chunks for an asset.
	DeleteByAssetID(ctx context.Context, assetID string) error
	// GetByIDs returns chunks matching the given primary-key IDs, ordered by
	// asset_id then chunk_index.
	GetByIDs(ctx context.Context, ids []string) ([]models.AssetChunk, error)
}

type assetChunksRepository struct {
	db *bun.DB
}

func NewAssetChunksRepository(db *bun.DB) AssetChunksRepository {
	return &assetChunksRepository{db: db}
}

func (r *assetChunksRepository) UpsertChunks(ctx context.Context, assetID string, chunks []models.AssetChunk) error {
	ptrs := make([]*models.AssetChunk, len(chunks))
	for i := range chunks {
		ptrs[i] = &chunks[i]
	}

	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		delRes, err := tx.NewDelete().
			Model((*models.AssetChunk)(nil)).
			Where("asset_id = ?", assetID).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("delete existing chunks: %w", err)
		}
		delN, _ := delRes.RowsAffected()

		if len(ptrs) == 0 {
			slog.InfoContext(ctx, "upserted chunks, nothing to insert", logging.AttrComponent, "repository.assets_chunks", "asset_id", assetID, "deleted", delN)
			return nil
		}

		insRes, err := tx.NewInsert().Model(&ptrs).Exec(ctx)
		if err != nil {
			return fmt.Errorf("insert chunks: %w", err)
		}
		insN, _ := insRes.RowsAffected()
		slog.InfoContext(ctx, "upserted chunks", logging.AttrComponent, "repository.assets_chunks", "asset_id", assetID, "deleted", delN, "inserted", insN, "wanted", len(ptrs))
		return nil
	})
}

func (r *assetChunksRepository) GetByAssetID(ctx context.Context, assetID string) ([]models.AssetChunk, error) {
	var chunks []models.AssetChunk
	err := r.db.NewSelect().
		Model(&chunks).
		Where("ac.asset_id = ?", assetID).
		OrderExpr("ac.chunk_index ASC").
		Scan(ctx)
	return chunks, err
}

func (r *assetChunksRepository) ListPageByAssetID(ctx context.Context, assetID string, offset, limit int) ([]models.AssetChunk, int, error) {
	chunks := []models.AssetChunk{}
	total, err := r.db.NewSelect().
		Model(&chunks).
		ExcludeColumn("embedding").
		Where("ac.asset_id = ?", assetID).
		OrderExpr("ac.chunk_index ASC").
		Offset(offset).
		Limit(limit).
		ScanAndCount(ctx)
	return chunks, total, err
}

func (r *assetChunksRepository) SearchSimilar(ctx context.Context, query pgvector.HalfVector, assetIDs []string, minScore float64, limit int) ([]models.AssetChunk, error) {
	var chunks []models.AssetChunk
	// `<=>` is pgvector's cosine-distance operator (0 = identical). Cosine
	// similarity is 1 - distance, so the threshold filter and the
	// closest-first ordering both key off the same expression.
	q := r.db.NewSelect().
		Model(&chunks).
		Where("ac.embedding IS NOT NULL").
		Where("(1 - (ac.embedding <=> ?)) >= ?", query, minScore).
		OrderExpr("ac.embedding <=> ?", query)
	if len(assetIDs) > 0 {
		q = q.Where("ac.asset_id IN (?)", bun.In(assetIDs))
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return chunks, nil
}

func (r *assetChunksRepository) DeleteByAssetID(ctx context.Context, assetID string) error {
	_, err := r.db.NewDelete().
		Model((*models.AssetChunk)(nil)).
		Where("asset_id = ?", assetID).
		Exec(ctx)
	return err
}

func (r *assetChunksRepository) GetByIDs(ctx context.Context, ids []string) ([]models.AssetChunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var chunks []models.AssetChunk
	err := r.db.NewSelect().
		Model(&chunks).
		Where("ac.id IN (?)", bun.In(ids)).
		OrderExpr("ac.asset_id ASC, ac.chunk_index ASC").
		Scan(ctx)
	return chunks, err
}
