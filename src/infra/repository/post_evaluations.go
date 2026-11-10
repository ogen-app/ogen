package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostEvaluationRepository persists the latest Post quality assessment.
// The table is one-to-one with Post via UNIQUE(post_id), so
// Upsert overwrites a prior evaluation rather than appending a new row —
// the spec stores only the most recent result.
type PostEvaluationRepository interface {
	// Upsert inserts the evaluation, or overwrites the existing row for
	// the same post_id. The caller supplies eval.ID (used on first insert;
	// ignored on conflict). updated_at is stamped here. Every call stands for
	// one model run: model_runs starts at 1 and is incremented on conflict,
	// and eval.ModelRuns is set to the stored count.
	Upsert(ctx context.Context, eval *models.PostEvaluation) error

	// GetByPostID returns the evaluation for a post, or (nil, nil) when the
	// post has not been evaluated yet.
	GetByPostID(ctx context.Context, postID string) (*models.PostEvaluation, error)

	// OverallByPost maps each evaluated post of postIDs to its overall score;
	// posts not evaluated yet are absent.
	OverallByPost(ctx context.Context, postIDs []string) (map[string]float64, error)
}

type postEvaluationRepository struct {
	db *bun.DB
}

func NewPostEvaluationRepository(db *bun.DB) PostEvaluationRepository {
	return &postEvaluationRepository{db: db}
}

func (r *postEvaluationRepository) Upsert(ctx context.Context, eval *models.PostEvaluation) error {
	eval.UpdatedAt = time.Now().UTC()
	eval.ModelRuns = 1
	_, err := r.db.NewInsert().
		Model(eval).
		On("CONFLICT (post_id) DO UPDATE").
		Set("platform_id = EXCLUDED.platform_id").
		Set("platform_post_type = EXCLUDED.platform_post_type").
		Set("caption_scoped = EXCLUDED.caption_scoped").
		Set("overall_pct = EXCLUDED.overall_pct").
		Set("result = EXCLUDED.result").
		Set("model_id = EXCLUDED.model_id").
		Set("input_hash = EXCLUDED.input_hash").
		Set("content_hash = EXCLUDED.content_hash").
		Set("context_hash = EXCLUDED.context_hash").
		Set("trigger = EXCLUDED.trigger").
		Set("model_runs = pe.model_runs + 1").
		Set("updated_at = EXCLUDED.updated_at").
		Returning("model_runs").
		Exec(ctx)
	return err
}

func (r *postEvaluationRepository) GetByPostID(ctx context.Context, postID string) (*models.PostEvaluation, error) {
	eval := new(models.PostEvaluation)
	err := r.db.NewSelect().
		Model(eval).
		Where("pe.post_id = ?", postID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return eval, nil
}

func (r *postEvaluationRepository) OverallByPost(ctx context.Context, postIDs []string) (map[string]float64, error) {
	out := map[string]float64{}
	if len(postIDs) == 0 {
		return out, nil
	}
	var rows []models.PostEvaluation
	if err := r.db.NewSelect().Model(&rows).Column("post_id", "overall_pct").
		Where("pe.post_id IN (?)", bun.List(postIDs)).Scan(ctx); err != nil {
		return nil, err
	}
	for _, e := range rows {
		out[e.PostID] = e.OverallPct
	}
	return out, nil
}
