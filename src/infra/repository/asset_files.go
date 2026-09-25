package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssetFileRepository defines persistence operations for AssetFile metadata.
type AssetFileRepository interface {
	GetByAssetID(ctx context.Context, assetID string) (*models.AssetFile, error)
	// GetByChecksum returns the caller-tenant's file row with this SHA-256, or
	// sql.ErrNoRows when none exists — the dedupe lookup behind image upload
	// (CON-246). Tenant scoping comes from the TenantScoped hooks.
	GetByChecksum(ctx context.Context, checksum string) (*models.AssetFile, error)
	Upsert(ctx context.Context, file *models.AssetFile) error
	DeleteByAssetID(ctx context.Context, assetID string) error
	ListByAssetIDs(ctx context.Context, assetIDs []string) (map[string]*models.AssetFile, error)
	// SumSizeBytesInTenant totals the stored originals across the ctx tenant's
	// content bank — its share of the media_storage_bytes quota (CON-312), beside
	// post attachments.
	SumSizeBytesInTenant(ctx context.Context) (int64, error)
}

type assetFileRepository struct {
	db *bun.DB
}

func NewAssetFileRepository(db *bun.DB) AssetFileRepository {
	return &assetFileRepository{db: db}
}

func (r *assetFileRepository) GetByAssetID(ctx context.Context, assetID string) (*models.AssetFile, error) {
	f := new(models.AssetFile)
	err := r.db.NewSelect().Model(f).Where("asset_id = ?", assetID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return f, nil
}

func (r *assetFileRepository) GetByChecksum(ctx context.Context, checksum string) (*models.AssetFile, error) {
	if checksum == "" {
		return nil, sql.ErrNoRows
	}
	f := new(models.AssetFile)
	err := r.db.NewSelect().
		Model(f).
		Where("checksum_sha256 = ?", checksum).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return f, nil
}

func (r *assetFileRepository) Upsert(ctx context.Context, file *models.AssetFile) error {
	file.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewInsert().
		Model(file).
		On("CONFLICT (asset_id) DO UPDATE").
		Set("original_name = EXCLUDED.original_name").
		Set("mime_type = EXCLUDED.mime_type").
		Set("size_bytes = EXCLUDED.size_bytes").
		Set("s3_key = EXCLUDED.s3_key").
		Set("thumbnail_s3_key = EXCLUDED.thumbnail_s3_key").
		Set("normalized_s3_key = EXCLUDED.normalized_s3_key").
		Set("page_count = EXCLUDED.page_count").
		Set("width = EXCLUDED.width").
		Set("height = EXCLUDED.height").
		Set("is_animated = EXCLUDED.is_animated").
		Set("checksum_sha256 = EXCLUDED.checksum_sha256").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (r *assetFileRepository) SumSizeBytesInTenant(ctx context.Context) (int64, error) {
	var total int64
	err := r.db.NewSelect().
		Model((*models.AssetFile)(nil)).
		ColumnExpr("COALESCE(SUM(af.size_bytes), 0)").
		Scan(ctx, &total)
	return total, err
}

func (r *assetFileRepository) DeleteByAssetID(ctx context.Context, assetID string) error {
	_, err := r.db.NewDelete().
		Model((*models.AssetFile)(nil)).
		Where("asset_id = ?", assetID).
		Exec(ctx)
	return err
}

func (r *assetFileRepository) ListByAssetIDs(ctx context.Context, assetIDs []string) (map[string]*models.AssetFile, error) {
	out := make(map[string]*models.AssetFile, len(assetIDs))
	if len(assetIDs) == 0 {
		return out, nil
	}
	var files []models.AssetFile
	err := r.db.NewSelect().Model(&files).Where("asset_id IN (?)", bun.In(assetIDs)).Scan(ctx)
	if err != nil {
		return nil, err
	}
	for i := range files {
		out[files[i].AssetID] = &files[i]
	}
	return out, nil
}
