package repository

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssetImageRepository persists the page images mirrored for URL-scraped assets
// (CON-222). One asset has many images, keyed by (asset_id, idx).
type AssetImageRepository interface {
	// ReplaceForAsset atomically replaces all images for the given asset
	// (delete-then-insert), so a refresh re-mirror leaves no stale rows.
	ReplaceForAsset(ctx context.Context, assetID string, images []models.AssetImage) error
	// GetByAssetID returns an asset's images ordered by idx.
	GetByAssetID(ctx context.Context, assetID string) ([]models.AssetImage, error)
	// DeleteByAssetID removes all images for an asset.
	DeleteByAssetID(ctx context.Context, assetID string) error
	// ListByAssetIDs returns images grouped by asset_id, each group ordered by idx.
	ListByAssetIDs(ctx context.Context, assetIDs []string) (map[string][]models.AssetImage, error)
}

type assetImageRepository struct {
	db *bun.DB
}

func NewAssetImageRepository(db *bun.DB) AssetImageRepository {
	return &assetImageRepository{db: db}
}

func (r *assetImageRepository) ReplaceForAsset(ctx context.Context, assetID string, images []models.AssetImage) error {
	ptrs := make([]*models.AssetImage, len(images))
	for i := range images {
		ptrs[i] = &images[i]
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*models.AssetImage)(nil)).
			Where("asset_id = ?", assetID).
			Exec(ctx); err != nil {
			return fmt.Errorf("delete existing images: %w", err)
		}
		if len(ptrs) == 0 {
			return nil
		}
		if _, err := tx.NewInsert().Model(&ptrs).Exec(ctx); err != nil {
			return fmt.Errorf("insert images: %w", err)
		}
		return nil
	})
}

func (r *assetImageRepository) GetByAssetID(ctx context.Context, assetID string) ([]models.AssetImage, error) {
	var images []models.AssetImage
	err := r.db.NewSelect().
		Model(&images).
		Where("aimg.asset_id = ?", assetID).
		OrderExpr("aimg.idx ASC").
		Scan(ctx)
	return images, err
}

func (r *assetImageRepository) DeleteByAssetID(ctx context.Context, assetID string) error {
	_, err := r.db.NewDelete().
		Model((*models.AssetImage)(nil)).
		Where("asset_id = ?", assetID).
		Exec(ctx)
	return err
}

func (r *assetImageRepository) ListByAssetIDs(ctx context.Context, assetIDs []string) (map[string][]models.AssetImage, error) {
	out := make(map[string][]models.AssetImage, len(assetIDs))
	if len(assetIDs) == 0 {
		return out, nil
	}
	var images []models.AssetImage
	err := r.db.NewSelect().
		Model(&images).
		Where("aimg.asset_id IN (?)", bun.List(assetIDs)).
		OrderExpr("aimg.asset_id ASC, aimg.idx ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	for i := range images {
		out[images[i].AssetID] = append(out[images[i].AssetID], images[i])
	}
	return out, nil
}
