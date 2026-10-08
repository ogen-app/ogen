package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// MediaPreviewRepository records the stored preview copies of post media.
type MediaPreviewRepository interface {
	// ListByKeys returns the previews fitting maxLongEdge that exist for keys,
	// by key.
	ListByKeys(ctx context.Context, maxLongEdge int, keys []models.MediaPreviewKey) (map[models.MediaPreviewKey]models.MediaPreview, error)
	// Create records p. A preview already recorded for the same key and size
	// is kept.
	Create(ctx context.Context, p *models.MediaPreview) error
}

type mediaPreviewRepository struct {
	db *bun.DB
}

// NewMediaPreviewRepository returns a Bun-backed MediaPreviewRepository.
func NewMediaPreviewRepository(db *bun.DB) MediaPreviewRepository {
	return &mediaPreviewRepository{db: db}
}

func (r *mediaPreviewRepository) ListByKeys(ctx context.Context, maxLongEdge int, keys []models.MediaPreviewKey) (map[models.MediaPreviewKey]models.MediaPreview, error) {
	out := make(map[models.MediaPreviewKey]models.MediaPreview, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	want := make(map[models.MediaPreviewKey]bool, len(keys))
	refs := make([]string, 0, len(keys))
	for _, k := range keys {
		if !want[k] {
			want[k] = true
			refs = append(refs, k.SourceRef)
		}
	}
	var rows []models.MediaPreview
	err := r.db.NewSelect().Model(&rows).
		Where("mp.max_long_edge = ?", maxLongEdge).
		Where("mp.source_ref IN (?)", bun.List(refs)).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		k := models.MediaPreviewKey{Source: row.Source, SourceRef: row.SourceRef}
		if want[k] {
			out[k] = row
		}
	}
	return out, nil
}

func (r *mediaPreviewRepository) Create(ctx context.Context, p *models.MediaPreview) error {
	_, err := r.db.NewInsert().Model(p).
		On("CONFLICT (tenant_id, source, source_ref, max_long_edge) DO NOTHING").
		Exec(ctx)
	return err
}
