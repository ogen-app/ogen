package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Query lists the posts matching q, oldest first, with their relations
// hydrated.
func (r *postRepository) Query(ctx context.Context, q models.PostQuery) ([]models.Post, error) {
	var posts []models.Post
	if err := postFilter(r.db.NewSelect().Model(&posts), q).OrderExpr("po.created_at ASC, po.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrateRelations(ctx, posts); err != nil {
		return nil, err
	}
	return posts, nil
}

// QueryIDs returns the ids of the first limit posts matching q, oldest first,
// and how many match in all. The total is a window over the same scan, not a
// separate Count: bun's Count skips the select hook that scopes the tenant.
// limit must be positive, since a page without rows carries no total.
func (r *postRepository) QueryIDs(ctx context.Context, q models.PostQuery, limit int) ([]string, int, error) {
	var rows []struct {
		ID    string `bun:"id"`
		Total int    `bun:"total"`
	}
	err := postFilter(r.db.NewSelect().Model((*models.Post)(nil)).Column("po.id").ColumnExpr("count(*) OVER () AS total"), q).
		OrderExpr("po.created_at ASC, po.id ASC").
		Limit(limit).
		Scan(ctx, &rows)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return []string{}, 0, nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids, rows[0].Total, nil
}

// postFilter narrows sel to the posts matching q.
func postFilter(sel *bun.SelectQuery, q models.PostQuery) *bun.SelectQuery {
	for col, f := range map[string]models.AssetValueFilter{
		"po.status": q.Status, "po.platform_id": q.Platform,
		"po.platform_post_type": q.PostType, "po.campaign_id": q.Campaign,
	} {
		if len(f.Any) > 0 {
			sel = sel.Where("? IN (?)", bun.Ident(col), bun.List(f.Any))
		}
		if len(f.Not) > 0 {
			// A post without the column set isn't one of the excluded values.
			sel = sel.Where("? IS NULL OR ? NOT IN (?)", bun.Ident(col), bun.Ident(col), bun.List(f.Not))
		}
	}
	if q.ScopeCampaign != "" {
		sel = sel.Where("po.campaign_id = ?", q.ScopeCampaign)
	}
	switch q.Scheduled {
	case models.PostScheduledYes:
		sel = sel.Where("po.scheduled_at IS NOT NULL")
	case models.PostScheduledNo:
		sel = sel.Where("po.scheduled_at IS NULL")
	}
	if q.ScheduledAfter != nil {
		sel = sel.Where("po.scheduled_at >= ?", *q.ScheduledAfter)
	}
	if q.ScheduledBefore != nil {
		sel = sel.Where("po.scheduled_at < ?", *q.ScheduledBefore)
	}
	return sel
}
