package repository

import (
	"context"
	"strings"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// MaxPostAttachTargets caps one attach-target listing.
const MaxPostAttachTargets = 50

// ListAttachTargets returns the tenant's posts that still accept new
// attachments (anything not yet submitted to a publisher), newest edit
// first, with only what a picker shows: no body text. A non-empty query
// matches the title case-insensitively. Posts of soft-deleted campaigns are
// left out. Scoped by the Post model's tenant hooks.
func (r *postRepository) ListAttachTargets(ctx context.Context, query string, limit int) ([]models.PostAttachTarget, error) {
	if limit <= 0 || limit > MaxPostAttachTargets {
		limit = MaxPostAttachTargets
	}
	out := []models.PostAttachTarget{}
	q := r.db.NewSelect().Model((*models.Post)(nil)).
		ColumnExpr("po.id, po.title, po.status, po.platform_id, po.campaign_id, po.updated_at").
		ColumnExpr("COALESCE(pl.name, '') AS platform_name").
		ColumnExpr("c.name AS campaign_name").
		ColumnExpr("(SELECT count(*) FROM post_attachments AS pa WHERE pa.post_id = po.id) AS attachment_count").
		Join("JOIN campaigns AS c ON c.id = po.campaign_id AND c.deleted_at IS NULL").
		Join("LEFT JOIN platforms AS pl ON pl.id = po.platform_id").
		Where("po.status NOT IN (?)", bun.List([]models.PostStatus{models.PostStatusScheduled, models.PostStatusPublished})).
		OrderExpr("po.updated_at DESC, po.id DESC").
		Limit(limit)
	if query = strings.TrimSpace(query); query != "" {
		q = q.Where(`po.title ILIKE ? ESCAPE '\'`, "%"+escapeLike(query)+"%")
	}
	if err := q.Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
