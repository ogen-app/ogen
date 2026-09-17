package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostLogRepository persists Post Log entries (CON-69 §11). Writers
// are expected to have already passed Payload through logs.Capper
// + logs.Sanitize — the repository performs no transformation,
// only persistence and querying.
type PostLogRepository interface {
	// Append inserts entry. If entry.ID is empty the caller is expected
	// to supply one; we intentionally do not generate it here so writers
	// can include the same ID in a structured log line emitted around
	// the same time.
	Append(ctx context.Context, entry *models.PostLog) error

	// AppendTx is the same as Append but uses the provided bun.IDB so
	// the write can join an outer transaction (CON-69 §11 — log entries
	// are written in the same transaction as the operation they
	// describe wherever possible). Passing nil falls back to the
	// repository's default DB.
	AppendTx(ctx context.Context, tx bun.IDB, entry *models.PostLog) error

	// ListByPostID returns entries for a single post in chronological
	// order. The default cap is 500 entries; pass limit > 0 to override.
	ListByPostID(ctx context.Context, postID string, limit int) ([]models.PostLog, error)

	// ListFiltered returns entries matching any combination of filters.
	// Empty values mean "ignore that filter". since/until are inclusive
	// boundaries on event_timestamp.
	ListFiltered(ctx context.Context, f PostLogFilter) ([]models.PostLog, error)

	// DeleteOlderThan removes entries with event_timestamp < cutoff.
	// Returns the number of rows deleted. Used by the cleanup_post_logs
	// recurring task (CON-69 plan §3).
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)

	// TerminalTransitionsBetween returns the tenant's post state-transitions INTO
	// a terminal non-publish state (failed / not_published) whose event_timestamp
	// falls in [from, to) — a zero from means unbounded-low — newest first,
	// optionally one campaign. limit 0 = no cap. It joins posts for the current
	// platform_id + failure_reason. This is the transition-time source for the
	// Activity report's "failed" bucket: a post that failed one local day and
	// succeeded on a retry the next still counts on the day it failed (CON-285
	// FR2/FR10), which a current-status read would miss.
	TerminalTransitionsBetween(ctx context.Context, from, to time.Time, campaignID string, limit int) ([]TerminalTransition, error)
}

// TerminalTransition is one post's move into a terminal non-publish state, as
// recorded in post_logs and joined to the post's current channel + failure
// reason (CON-285). failure_reason is the post's current value (best-effort for
// a historical transition); status is the transition's to_status.
type TerminalTransition struct {
	PostID        string    `bun:"post_id"`
	PlatformID    string    `bun:"platform_id"`
	Status        string    `bun:"to_status"`
	FailureReason string    `bun:"failure_reason"`
	At            time.Time `bun:"event_timestamp"`
}

// PostLogFilter is the search shape for ListFiltered. Zero-valued
// fields are ignored.
type PostLogFilter struct {
	PostID    string
	EventType models.PostLogEventType
	Actor     string
	Since     time.Time
	Until     time.Time
	Limit     int
}

type postLogRepository struct {
	db *bun.DB
}

func NewPostLogRepository(db *bun.DB) PostLogRepository {
	return &postLogRepository{db: db}
}

func (r *postLogRepository) Append(ctx context.Context, entry *models.PostLog) error {
	return r.AppendTx(ctx, nil, entry)
}

func (r *postLogRepository) AppendTx(ctx context.Context, tx bun.IDB, entry *models.PostLog) error {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	if entry.EventTimestamp.IsZero() {
		entry.EventTimestamp = time.Now().UTC()
	}
	if entry.Payload == "" {
		entry.Payload = "{}"
	}
	_, err := db.NewInsert().Model(entry).Exec(ctx)
	return err
}

func (r *postLogRepository) ListByPostID(ctx context.Context, postID string, limit int) ([]models.PostLog, error) {
	if limit <= 0 {
		limit = 500
	}
	var logs []models.PostLog
	err := r.db.NewSelect().
		Model(&logs).
		Where("pl.post_id = ?", postID).
		OrderExpr("event_timestamp ASC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return logs, nil
}

func (r *postLogRepository) ListFiltered(ctx context.Context, f PostLogFilter) ([]models.PostLog, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := r.db.NewSelect().Model((*models.PostLog)(nil))
	if f.PostID != "" {
		q = q.Where("post_id = ?", f.PostID)
	}
	if f.EventType != "" {
		q = q.Where("event_type = ?", string(f.EventType))
	}
	if f.Actor != "" {
		q = q.Where("actor = ?", f.Actor)
	}
	if !f.Since.IsZero() {
		q = q.Where("event_timestamp >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		q = q.Where("event_timestamp <= ?", f.Until)
	}
	var logs []models.PostLog
	err := q.OrderExpr("event_timestamp DESC").Limit(limit).Scan(ctx, &logs)
	if err != nil {
		return nil, err
	}
	return logs, nil
}

func (r *postLogRepository) TerminalTransitionsBetween(ctx context.Context, from, to time.Time, campaignID string, limit int) ([]TerminalTransition, error) {
	var rows []TerminalTransition
	q := r.db.NewSelect().
		Model((*models.PostLog)(nil)).
		ColumnExpr("pl.post_id AS post_id").
		ColumnExpr("po.platform_id AS platform_id").
		ColumnExpr("pl.to_status AS to_status").
		ColumnExpr("po.failure_reason AS failure_reason").
		ColumnExpr("pl.event_timestamp AS event_timestamp").
		Join("JOIN posts AS po ON po.id = pl.post_id").
		Where("pl.event_type = ?", string(models.PostLogEventStateTransition)).
		Where("pl.to_status IN (?)", bun.In([]string{
			string(models.PostStatusFailed),
			string(models.PostStatusNotPublished),
		})).
		Where("pl.event_timestamp < ?", to)
	if !from.IsZero() {
		q = q.Where("pl.event_timestamp >= ?", from)
	}
	if campaignID != "" {
		q = q.Where("po.campaign_id = ?", campaignID)
	}
	q = q.OrderExpr("pl.event_timestamp DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	// pl carries the tenant predicate via TenantScoped.BeforeSelect
	// (?TableAlias.tenant_id), so the join to posts stays within the tenant.
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *postLogRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.NewDelete().
		Model((*models.PostLog)(nil)).
		Where("event_timestamp < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
