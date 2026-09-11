package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ImageExtractionRepository persists the per-run image vision state (CON-281).
// Tenant scoping comes from the TenantScoped hooks on the model.
type ImageExtractionRepository interface {
	// Create inserts a new extraction row. A duplicate (asset_id, run_key) hits
	// the unique index and returns an error the caller treats as "already
	// enqueued" (idempotency).
	Create(ctx context.Context, e *models.ImageExtraction) error
	// GetByAssetAndRunKey returns the extraction for an (asset, run_key), or
	// sql.ErrNoRows when none exists.
	GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*models.ImageExtraction, error)
	// GetLatestByAsset returns the most recent extraction for an asset (the
	// status/extraction API reads the latest run), or sql.ErrNoRows.
	GetLatestByAsset(ctx context.Context, assetID string) (*models.ImageExtraction, error)
	// Update saves the mutable fields of an extraction (status, shape, quality
	// flags, cost snapshot); updated_at is bumped.
	Update(ctx context.Context, e *models.ImageExtraction) error
}

type imageExtractionRepository struct {
	db *bun.DB
}

func NewImageExtractionRepository(db *bun.DB) ImageExtractionRepository {
	return &imageExtractionRepository{db: db}
}

func (r *imageExtractionRepository) Create(ctx context.Context, e *models.ImageExtraction) error {
	_, err := r.db.NewInsert().Model(e).Exec(ctx)
	return err
}

func (r *imageExtractionRepository) GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*models.ImageExtraction, error) {
	e := new(models.ImageExtraction)
	err := r.db.NewSelect().Model(e).
		Where("asset_id = ?", assetID).
		Where("run_key = ?", runKey).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return e, nil
}

func (r *imageExtractionRepository) GetLatestByAsset(ctx context.Context, assetID string) (*models.ImageExtraction, error) {
	e := new(models.ImageExtraction)
	err := r.db.NewSelect().Model(e).
		Where("asset_id = ?", assetID).
		OrderExpr("created_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return e, nil
}

func (r *imageExtractionRepository) Update(ctx context.Context, e *models.ImageExtraction) error {
	e.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(e).WherePK().Exec(ctx)
	return err
}
