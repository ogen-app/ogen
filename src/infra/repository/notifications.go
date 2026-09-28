package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// NotificationListOpts filters a user's inbox read. Every read is
// scoped to the caller's user_id AND the active tenant (the TenantScoped hook
// adds the tenant predicate); dismissed rows are always excluded. Because a
// user_id (membership) maps 1:1 to a tenant, the user_id predicate alone is
// sufficient for correctness — the tenant hook is belt-and-suspenders.
type NotificationListOpts struct {
	UnreadOnly bool
	Limit      int
	// BeforeSeq (exclusive) pages backwards into older notifications; SinceSeq
	// (exclusive) fetches only newer ones. Zero disables that bound.
	BeforeSeq int64
	SinceSeq  int64
}

// NotificationRepository persists the per-user notification inbox.
// Every query runs through bun's Model API so the TenantScoped hooks scope
// tenant_id; the explicit user_id predicate narrows to the caller.
type NotificationRepository interface {
	// Insert writes one notification, returning whether a row was actually
	// created (false when a dedupe_key collision was swallowed by ON CONFLICT DO
	// NOTHING). On success the model's Seq and CreatedAt are populated from
	// RETURNING.
	Insert(ctx context.Context, n *models.Notification) (bool, error)
	// List returns the caller's non-dismissed notifications, newest-first (seq
	// DESC), honouring the filter/pagination opts.
	List(ctx context.Context, userID string, opts NotificationListOpts) ([]models.Notification, error)
	// ReplaySince returns the caller's non-dismissed notifications with seq >
	// sinceSeq in ASCENDING seq order — the SSE reconnect catch-up (FR9).
	ReplaySince(ctx context.Context, userID string, sinceSeq int64, limit int) ([]models.Notification, error)
	// UnreadCount is the badge count over live, unread rows.
	UnreadCount(ctx context.Context, userID string) (int, error)
	// Get fetches one of the caller's notifications; sql.ErrNoRows when absent.
	Get(ctx context.Context, userID, id string) (*models.Notification, error)
	// SetRead toggles a single row's read state, returning whether it matched.
	SetRead(ctx context.Context, userID, id string, read bool) (bool, error)
	// MarkAllRead marks every unread row read, optionally bounded to seq <=
	// beforeSeq (0 = all), returning the number updated.
	MarkAllRead(ctx context.Context, userID string, beforeSeq int64) (int, error)
	// Dismiss soft-deletes a single row (sets dismissed_at), returning whether it matched.
	Dismiss(ctx context.Context, userID, id string) (bool, error)
	// DeleteExpired reaps expired rows plus read/dismissed rows older than
	// retention, across ALL tenants — callers pass a system context. Returns the
	// number removed. retention <= 0 reaps only expired rows.
	DeleteExpired(ctx context.Context, now time.Time, retention time.Duration) (int64, error)
}

type notificationRepository struct {
	db *bun.DB
}

func NewNotificationRepository(db *bun.DB) NotificationRepository {
	return &notificationRepository{db: db}
}

func (r *notificationRepository) Insert(ctx context.Context, n *models.Notification) (bool, error) {
	res, err := r.db.NewInsert().
		Model(n).
		On("CONFLICT DO NOTHING").
		Returning("seq, created_at").
		Exec(ctx)
	if err != nil {
		// A dedupe_key collision under ON CONFLICT DO NOTHING returns no row; bun
		// surfaces the empty RETURNING as ErrNoRows. That's a swallowed duplicate,
		// not a failure.
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

func (r *notificationRepository) List(ctx context.Context, userID string, opts NotificationListOpts) ([]models.Notification, error) {
	var out []models.Notification
	q := r.db.NewSelect().Model(&out).
		Where("n.user_id = ?", userID).
		Where("n.dismissed_at IS NULL").
		// Expired notifications fade from the inbox at expires_at, independently
		// of the (slower) cleanup sweep.
		Where("(n.expires_at IS NULL OR n.expires_at > now())")
	if opts.UnreadOnly {
		q = q.Where("n.read_at IS NULL")
	}
	if opts.BeforeSeq > 0 {
		q = q.Where("n.seq < ?", opts.BeforeSeq)
	}
	if opts.SinceSeq > 0 {
		q = q.Where("n.seq > ?", opts.SinceSeq)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	} else if limit > 100 {
		limit = 100
	}
	if err := q.Order("n.seq DESC").Limit(limit).Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *notificationRepository) ReplaySince(ctx context.Context, userID string, sinceSeq int64, limit int) ([]models.Notification, error) {
	var out []models.Notification
	q := r.db.NewSelect().Model(&out).
		Where("n.user_id = ?", userID).
		Where("n.dismissed_at IS NULL").
		Where("(n.expires_at IS NULL OR n.expires_at > now())")
	if sinceSeq > 0 {
		q = q.Where("n.seq > ?", sinceSeq)
	}
	if limit <= 0 {
		limit = 200
	}
	if err := q.Order("n.seq ASC").Limit(limit).Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *notificationRepository) UnreadCount(ctx context.Context, userID string) (int, error) {
	return r.db.NewSelect().Model((*models.Notification)(nil)).
		Where("n.user_id = ?", userID).
		Where("n.read_at IS NULL").
		Where("n.dismissed_at IS NULL").
		Where("(n.expires_at IS NULL OR n.expires_at > now())").
		Count(ctx)
}

func (r *notificationRepository) Get(ctx context.Context, userID, id string) (*models.Notification, error) {
	n := new(models.Notification)
	err := r.db.NewSelect().Model(n).
		Where("n.id = ?", id).
		Where("n.user_id = ?", userID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (r *notificationRepository) SetRead(ctx context.Context, userID, id string, read bool) (bool, error) {
	q := r.db.NewUpdate().Model((*models.Notification)(nil)).
		Where("n.id = ?", id).
		Where("n.user_id = ?", userID)
	if read {
		q = q.Set("read_at = ?", time.Now().UTC())
	} else {
		q = q.Set("read_at = NULL")
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *notificationRepository) MarkAllRead(ctx context.Context, userID string, beforeSeq int64) (int, error) {
	q := r.db.NewUpdate().Model((*models.Notification)(nil)).
		Set("read_at = ?", time.Now().UTC()).
		Where("n.user_id = ?", userID).
		Where("n.read_at IS NULL").
		Where("n.dismissed_at IS NULL")
	if beforeSeq > 0 {
		q = q.Where("n.seq <= ?", beforeSeq)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *notificationRepository) Dismiss(ctx context.Context, userID, id string) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.Notification)(nil)).
		Set("dismissed_at = ?", time.Now().UTC()).
		Where("n.id = ?", id).
		Where("n.user_id = ?", userID).
		Where("n.dismissed_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *notificationRepository) DeleteExpired(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	q := r.db.NewDelete().Model((*models.Notification)(nil))
	if retention > 0 {
		cutoff := now.Add(-retention)
		q = q.Where(
			"(n.expires_at IS NOT NULL AND n.expires_at < ?) OR (n.read_at IS NOT NULL AND n.read_at < ?) OR (n.dismissed_at IS NOT NULL AND n.dismissed_at < ?)",
			now, cutoff, cutoff,
		)
	} else {
		q = q.Where("n.expires_at IS NOT NULL AND n.expires_at < ?", now)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
