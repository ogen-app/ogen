package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AudioExtractionRepository persists the per-run audio transcription state
// (CON-282). Tenant scoping comes from the TenantScoped hooks on the model.
type AudioExtractionRepository interface {
	// Create inserts a new extraction row. A duplicate (asset_id, run_key) hits
	// the unique index and returns an error the caller treats as "already
	// enqueued" (idempotency).
	Create(ctx context.Context, e *models.AudioExtraction) error
	// GetByAssetAndRunKey returns the extraction for an (asset, run_key), or
	// sql.ErrNoRows when none exists.
	GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*models.AudioExtraction, error)
	// GetLatestByAsset returns the most recent extraction for an asset (the
	// status/transcript API reads the latest run), or sql.ErrNoRows.
	GetLatestByAsset(ctx context.Context, assetID string) (*models.AudioExtraction, error)
	// Update saves the mutable fields of an extraction (status, progress,
	// cost snapshot); updated_at is bumped.
	Update(ctx context.Context, e *models.AudioExtraction) error
}

type audioExtractionRepository struct {
	db *bun.DB
}

func NewAudioExtractionRepository(db *bun.DB) AudioExtractionRepository {
	return &audioExtractionRepository{db: db}
}

func (r *audioExtractionRepository) Create(ctx context.Context, e *models.AudioExtraction) error {
	_, err := r.db.NewInsert().Model(e).Exec(ctx)
	return err
}

func (r *audioExtractionRepository) GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*models.AudioExtraction, error) {
	e := new(models.AudioExtraction)
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

func (r *audioExtractionRepository) GetLatestByAsset(ctx context.Context, assetID string) (*models.AudioExtraction, error) {
	e := new(models.AudioExtraction)
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

func (r *audioExtractionRepository) Update(ctx context.Context, e *models.AudioExtraction) error {
	e.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(e).WherePK().Exec(ctx)
	return err
}
