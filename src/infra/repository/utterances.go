package repository

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// UtteranceRepository persists the raw transcript spans of an extraction
// (CON-282): the source for both chunk assembly and the transcript API.
type UtteranceRepository interface {
	// ReplaceForSegment atomically replaces a segment's utterances, so a
	// segment retry is idempotent (old spans dropped, fresh ones written).
	ReplaceForSegment(ctx context.Context, segmentID string, utterances []models.Utterance) error
	// ListByExtraction returns a single extraction run's utterances in timeline
	// order (start_ms, then index), scoped through that run's segments. Keying on
	// the extraction (not the asset) keeps a re-extraction from reading a prior
	// run's leftover spans — the transcript read and chunk-assembly source.
	ListByExtraction(ctx context.Context, extractionID string) ([]models.Utterance, error)
}

type utteranceRepository struct {
	db *bun.DB
}

func NewUtteranceRepository(db *bun.DB) UtteranceRepository {
	return &utteranceRepository{db: db}
}

func (r *utteranceRepository) ReplaceForSegment(ctx context.Context, segmentID string, utterances []models.Utterance) error {
	ptrs := make([]*models.Utterance, len(utterances))
	for i := range utterances {
		ptrs[i] = &utterances[i]
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*models.Utterance)(nil)).
			Where("segment_id = ?", segmentID).
			Exec(ctx); err != nil {
			return fmt.Errorf("delete existing utterances: %w", err)
		}
		if len(ptrs) == 0 {
			return nil
		}
		if _, err := tx.NewInsert().Model(&ptrs).Exec(ctx); err != nil {
			return fmt.Errorf("insert utterances: %w", err)
		}
		return nil
	})
}

func (r *utteranceRepository) ListByExtraction(ctx context.Context, extractionID string) ([]models.Utterance, error) {
	var utterances []models.Utterance
	// Scope through the run's segments. extraction_id is tenant-specific and the
	// outer query is tenant-scoped by the TenantScoped hook, so the subquery can't
	// leak across tenants.
	err := r.db.NewSelect().Model(&utterances).
		Where("segment_id IN (SELECT id FROM audio_segments WHERE extraction_id = ?)", extractionID).
		OrderExpr("start_ms ASC, index ASC").
		Scan(ctx)
	return utterances, err
}
