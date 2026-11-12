package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// scheduledStatuses are the posts waiting to go out: on their own, or by
// hand.
var scheduledStatuses = []models.PostStatus{models.PostStatusScheduled, models.PostStatusScheduledForManualPublish}

// ScheduledCount is how many posts wait to go out on one platform, through
// one account, by one route: scheduled to go out on their own, or by hand.
// AccountID is empty for posts that name none.
type ScheduledCount struct {
	PlatformID string            `bun:"platform_id"`
	AccountID  string            `bun:"social_account_id"`
	Status     models.PostStatus `bun:"status"`
	Count      int               `bun:"n"`
}

// NextScheduled is the first post waiting to go out after after, in
// campaignID when it is set; nil when none is.
func (r *postRepository) NextScheduled(ctx context.Context, after time.Time, campaignID string) (*models.Post, error) {
	post := new(models.Post)
	sel := r.db.NewSelect().Model(post).
		Column("po.id", "po.title", "po.platform_id", "po.campaign_id", "po.scheduled_at", "po.status").
		Where("po.status IN (?)", bun.List(scheduledStatuses)).
		Where("po.scheduled_at > ?", after).
		OrderExpr("po.scheduled_at ASC, po.id ASC").
		Limit(1)
	if campaignID != "" {
		sel = sel.Where("po.campaign_id = ?", campaignID)
	}
	if err := sel.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return post, nil
}

// ScheduledCounts counts the posts waiting to go out by platform, account
// and route.
func (r *postRepository) ScheduledCounts(ctx context.Context) ([]ScheduledCount, error) {
	var rows []ScheduledCount
	err := r.db.NewSelect().Model((*models.Post)(nil)).
		ColumnExpr("po.platform_id, coalesce(po.social_account_id, '') AS social_account_id, po.status, count(*) AS n").
		Where("po.status IN (?)", bun.List(scheduledStatuses)).
		GroupExpr("po.platform_id, po.social_account_id, po.status").
		OrderExpr("po.platform_id, social_account_id, po.status").
		Scan(ctx, &rows)
	return rows, err
}
