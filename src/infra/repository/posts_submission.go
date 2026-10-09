package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// UpdateSubmission writes the named columns of post only while the row is
// still scheduled and still holds heldID, the publisher_post_id the caller
// loaded ("" for a post not yet submitted). It reports false when another
// writer moved the post on in between: a cancel landed, the post was deleted,
// or a newer submission replaced this one. Publish workers write through it so
// a slow job never restores a post the user has since unscheduled.
func (r *postRepository) UpdateSubmission(ctx context.Context, post *models.Post, heldID string, columns ...string) (bool, error) {
	res, err := r.db.NewUpdate().Model(post).
		Column(columns...).
		Where("status = ?", models.PostStatusScheduled).
		Where("COALESCE(publisher_post_id, '') = ?", heldID).
		WherePK().
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SettleFirstComment writes the named columns of post only while it is still
// published under the same Zernio post and its first comment is still pending.
// It reports false when the comment was already settled or the post moved on,
// so a duplicate or late worker never overwrites an outcome.
func (r *postRepository) SettleFirstComment(ctx context.Context, post *models.Post, columns ...string) (bool, error) {
	res, err := r.db.NewUpdate().Model(post).
		Column(columns...).
		Where("status = ?", models.PostStatusPublished).
		Where("first_comment_status = ?", models.FirstCommentPending).
		Where("publisher_post_id = ?", post.PublisherPostID).
		WherePK().
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateWhileScheduled writes the whole record (less excludeColumns) under the
// same condition as UpdateSubmission: the row is still scheduled and still
// holds heldID. It backs an edit of a scheduled post, which must not restore
// the post if a cancel or publish landed while the edit was in flight.
func (r *postRepository) UpdateWhileScheduled(ctx context.Context, post *models.Post, heldID string, excludeColumns ...string) (bool, error) {
	q := r.db.NewUpdate().Model(post).
		Where("status = ?", models.PostStatusScheduled).
		Where("COALESCE(publisher_post_id, '') = ?", heldID).
		WherePK()
	if len(excludeColumns) > 0 {
		q = q.ExcludeColumn(excludeColumns...)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ListByPublisherPostIDs returns id, tenant_id, status and publisher_post_id of
// the posts holding any of the given publisher post ids. Ids no post holds are
// absent from the result.
func (r *postRepository) ListByPublisherPostIDs(ctx context.Context, ids []string) ([]models.Post, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var posts []models.Post
	err := r.db.NewSelect().
		Model(&posts).
		Column("id", "tenant_id", "status", "publisher_post_id").
		Where("po.publisher_post_id IN (?)", bun.List(ids)).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return posts, nil
}

// DeleteTx hard-deletes one post on db, which may be a transaction, and returns
// its id, tenant_id, status and publisher_post_id as they were at the moment of
// the delete, or nil when no such post exists. Callers decide what the delete
// orphans from this row, not from an earlier read a concurrent schedule or
// publish may have overtaken.
func (r *postRepository) DeleteTx(ctx context.Context, db bun.IDB, id string) (*models.Post, error) {
	var deleted []models.Post
	err := db.NewDelete().Model(&deleted).
		Where("id = ?", id).
		Returning("id, tenant_id, status, publisher_post_id").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	if len(deleted) == 0 {
		return nil, nil
	}
	return &deleted[0], nil
}

// UnscheduleByCampaignTx moves every scheduled or manually-scheduled post of a
// campaign back to draft and drops its publisher identity, on db (normally
// the campaign-delete transaction). It returns the rows as they were before
// the change, so the caller can withdraw each publisher_post_id from Zernio.
func (r *postRepository) UnscheduleByCampaignTx(ctx context.Context, db bun.IDB, campaignID string) ([]models.Post, error) {
	var posts []models.Post
	err := db.NewSelect().
		Model(&posts).
		Column("id", "tenant_id", "status", "publisher_post_id").
		Where("po.campaign_id = ?", campaignID).
		Where("po.status IN (?)", bun.List([]models.PostStatus{
			models.PostStatusScheduled, models.PostStatusScheduledForManualPublish,
		})).
		For("UPDATE").
		Scan(ctx)
	if err != nil || len(posts) == 0 {
		return nil, err
	}
	ids := make([]string, len(posts))
	for i := range posts {
		ids[i] = posts[i].ID
	}
	_, err = db.NewUpdate().Model((*models.Post)(nil)).
		Set("status = ?", models.PostStatusDraft).
		Set("publisher_post_id = NULL").
		Set("publisher_status = ''").
		Set("updated_at = ?", time.Now().UTC()).
		Where("id IN (?)", bun.List(ids)).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	return posts, nil
}
