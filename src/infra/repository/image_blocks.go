package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ImageBlockRepository persists the structured blocks extracted from a
// content-bank image (CON-281). Blocks are additive per run and replaced
// wholesale when a run is re-driven, so ReplaceForExtraction is the write path.
type ImageBlockRepository interface {
	// ReplaceForExtraction deletes any existing blocks for the extraction and
	// inserts the given set in one transaction, making a job retry idempotent.
	ReplaceForExtraction(ctx context.Context, extractionID string, blocks []models.ImageBlock) error
	// ListByExtraction returns an extraction's blocks in index order.
	ListByExtraction(ctx context.Context, extractionID string) ([]models.ImageBlock, error)
}

type imageBlockRepository struct {
	db *bun.DB
}

func NewImageBlockRepository(db *bun.DB) ImageBlockRepository {
	return &imageBlockRepository{db: db}
}

func (r *imageBlockRepository) ReplaceForExtraction(ctx context.Context, extractionID string, blocks []models.ImageBlock) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*models.ImageBlock)(nil)).
			Where("extraction_id = ?", extractionID).
			Exec(ctx); err != nil {
			return err
		}
		if len(blocks) == 0 {
			return nil
		}
		_, err := tx.NewInsert().Model(&blocks).Exec(ctx)
		return err
	})
}

func (r *imageBlockRepository) ListByExtraction(ctx context.Context, extractionID string) ([]models.ImageBlock, error) {
	var blocks []models.ImageBlock
	err := r.db.NewSelect().
		Model(&blocks).
		Where("extraction_id = ?", extractionID).
		OrderExpr("index ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return blocks, nil
}
