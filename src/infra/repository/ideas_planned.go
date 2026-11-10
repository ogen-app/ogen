package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PlannedIdeas counts with a scanned count(*): bun's Count skips the
// BeforeSelect hook, so it would count every tenant's ideas.
func (r *ideaRepository) PlannedIdeas(ctx context.Context, campaignID string, limit int) ([]models.Idea, int, error) {
	planned := func(q *bun.SelectQuery) *bun.SelectQuery {
		q = q.Where("i.verdict IS DISTINCT FROM ?", models.IdeaVerdictNo).
			Where("NOT EXISTS (SELECT 1 FROM post_ideas AS pi WHERE pi.idea_id = i.id AND pi.tenant_id = i.tenant_id)").
			Where("(i.plan_platform_id IS NOT NULL OR i.plan_post_type IS NOT NULL OR i.plan_phase_id IS NOT NULL OR i.plan_target_date IS NOT NULL)")
		if campaignID != "" {
			return q.Where("c.id = ?", campaignID)
		}
		return q.Where("c.archived_at IS NULL")
	}
	var total int
	if err := planned(r.db.NewSelect().Model((*models.Idea)(nil)).ColumnExpr("count(*)").
		Join("LEFT JOIN campaigns AS c ON c.id = i.campaign_id AND c.deleted_at IS NULL")).
		Scan(ctx, &total); err != nil {
		return nil, 0, err
	}
	ideas := []models.Idea{}
	if total == 0 {
		return ideas, 0, nil
	}
	err := planned(r.selectIdeas(&ideas)).
		OrderExpr("i.plan_target_date ASC NULLS LAST, i.created_at DESC, i.id").
		Limit(limit).
		Scan(ctx)
	return ideas, total, err
}
