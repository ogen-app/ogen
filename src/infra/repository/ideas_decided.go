package repository

import (
	"context"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// CountDecided counts the ideas decided in [from, to), in campaignID when it
// is set. A scan, not bun's Count, which skips the select hook that scopes
// the tenant.
func (r *ideaRepository) CountDecided(ctx context.Context, from, to time.Time, campaignID string) (int, error) {
	var n int
	sel := r.db.NewSelect().Model((*models.Idea)(nil)).ColumnExpr("count(*)").
		Where("?TableAlias.decided_at >= ?", from).
		Where("?TableAlias.decided_at < ?", to)
	if campaignID != "" {
		sel = sel.Where("?TableAlias.campaign_id = ?", campaignID)
	}
	err := sel.Scan(ctx, &n)
	return n, err
}
