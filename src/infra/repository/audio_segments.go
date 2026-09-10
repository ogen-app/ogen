package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AudioSegmentRepository persists the bounded transcription windows of an
// extraction (CON-282). Segments are checkpointed so a retry resumes from the
// first incomplete one.
type AudioSegmentRepository interface {
	// CreateMany inserts the initial (pending) segment set for an extraction.
	// Idempotent across retries via the unique (extraction_id, index) index:
	// ON CONFLICT DO NOTHING keeps a re-drive from duplicating rows.
	CreateMany(ctx context.Context, segments []models.AudioSegment) error
	// ListByExtraction returns all segments of an extraction, ordered by index.
	ListByExtraction(ctx context.Context, extractionID string) ([]models.AudioSegment, error)
	// Update saves a segment's mutable fields (status, retry_count,
	// failure_reason, utterance_count); updated_at is bumped.
	Update(ctx context.Context, s *models.AudioSegment) error
	// ResetFailed flips an extraction's failed segments back to pending (clearing
	// the failure reason) so a retry re-drives only them; returns how many were
	// reset. Done segments are untouched.
	ResetFailed(ctx context.Context, extractionID string) (int, error)
}

type audioSegmentRepository struct {
	db *bun.DB
}

func NewAudioSegmentRepository(db *bun.DB) AudioSegmentRepository {
	return &audioSegmentRepository{db: db}
}

func (r *audioSegmentRepository) CreateMany(ctx context.Context, segments []models.AudioSegment) error {
	if len(segments) == 0 {
		return nil
	}
	ptrs := make([]*models.AudioSegment, len(segments))
	for i := range segments {
		ptrs[i] = &segments[i]
	}
	_, err := r.db.NewInsert().Model(&ptrs).
		On("CONFLICT (extraction_id, index) DO NOTHING").
		Exec(ctx)
	return err
}

func (r *audioSegmentRepository) ListByExtraction(ctx context.Context, extractionID string) ([]models.AudioSegment, error) {
	var segments []models.AudioSegment
	err := r.db.NewSelect().Model(&segments).
		Where("extraction_id = ?", extractionID).
		OrderExpr("index ASC").
		Scan(ctx)
	return segments, err
}

func (r *audioSegmentRepository) Update(ctx context.Context, s *models.AudioSegment) error {
	s.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(s).WherePK().Exec(ctx)
	return err
}

func (r *audioSegmentRepository) ResetFailed(ctx context.Context, extractionID string) (int, error) {
	res, err := r.db.NewUpdate().
		Model((*models.AudioSegment)(nil)).
		Set("status = ?", models.AudioSegmentStatusPending).
		Set("failure_reason = ''").
		Set("updated_at = ?", time.Now().UTC()).
		Where("extraction_id = ?", extractionID).
		Where("status = ?", models.AudioSegmentStatusFailed).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
