package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// QueuedAssessment is a claimed post_quality_queue row. DueAt identifies the
// claim: a checkpoint that lands while the run is in flight moves due_at, and
// Finish then keeps the row for another run instead of deleting it.
type QueuedAssessment struct {
	PostID   string                `bun:"post_id"`
	TenantID string                `bun:"tenant_id"`
	Trigger  models.QualityTrigger `bun:"trigger"`
	DueAt    time.Time             `bun:"due_at"`
	Attempts int                   `bun:"attempts"`
}

// PostQualityQueueRepository holds the pending automatic quality assessments,
// one per post. The queue spans tenants: rows carry their tenant and the
// sweep sets it on the context of each run.
type PostQualityQueueRepository interface {
	// Schedule queues post for assessment at dueAt, replacing the post's
	// pending trigger and due time. A claim in flight is left alone.
	Schedule(ctx context.Context, tenantID, postID string, trigger models.QualityTrigger, dueAt time.Time) error
	// Claim leases up to limit rows due at now until now+lease, oldest due
	// first, skipping rows another sweep holds, and counts the attempt.
	Claim(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]QueuedAssessment, error)
	// Finish removes a claimed row, or only releases the claim when a newer
	// checkpoint moved its due time.
	Finish(ctx context.Context, q QueuedAssessment) error
	// Retry releases a claimed row to run again at next, unless a newer
	// checkpoint already moved its due time.
	Retry(ctx context.Context, q QueuedAssessment, next time.Time) error
}

type postQualityQueueRepository struct {
	db *bun.DB
}

func NewPostQualityQueueRepository(db *bun.DB) PostQualityQueueRepository {
	return &postQualityQueueRepository{db: db}
}

func (r *postQualityQueueRepository) Schedule(ctx context.Context, tenantID, postID string, trigger models.QualityTrigger, dueAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO post_quality_queue (post_id, tenant_id, trigger, due_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (post_id) DO UPDATE
		SET trigger = EXCLUDED.trigger, due_at = EXCLUDED.due_at, attempts = 0, updated_at = now()`,
		postID, tenantID, trigger, dueAt.UTC())
	return err
}

func (r *postQualityQueueRepository) Claim(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]QueuedAssessment, error) {
	var rows []QueuedAssessment
	err := r.db.NewRaw(`
		UPDATE post_quality_queue q
		SET claimed_until = ?, attempts = q.attempts + 1, updated_at = now()
		WHERE q.post_id IN (
			SELECT post_id FROM post_quality_queue
			WHERE due_at <= ? AND (claimed_until IS NULL OR claimed_until < ?)
			ORDER BY due_at
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING q.post_id, q.tenant_id, q.trigger, q.due_at, q.attempts`,
		now.Add(lease).UTC(), now.UTC(), now.UTC(), limit).
		Scan(ctx, &rows)
	return rows, err
}

func (r *postQualityQueueRepository) Finish(ctx context.Context, q QueuedAssessment) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM post_quality_queue WHERE post_id = ? AND due_at = ?`, q.PostID, q.DueAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE post_quality_queue SET claimed_until = NULL, updated_at = now() WHERE post_id = ?`, q.PostID)
	return err
}

func (r *postQualityQueueRepository) Retry(ctx context.Context, q QueuedAssessment, next time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE post_quality_queue
		SET due_at = CASE WHEN due_at = ? THEN ? ELSE due_at END,
		    claimed_until = NULL, updated_at = now()
		WHERE post_id = ?`,
		q.DueAt, next.UTC(), q.PostID)
	return err
}
