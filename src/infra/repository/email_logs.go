package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// EmailListFilter parameterises a tenant-scoped, keyset-paginated email list
// (CON-298). A zero cursor (empty CursorID) requests the first page; otherwise
// rows strictly older than (CursorCreatedAt, CursorID) in newest-first order are
// returned. All filter fields are optional (empty = any).
type EmailListFilter struct {
	TenantID        string
	Statuses        []models.EmailLogStatus
	Kind            models.EmailKind
	Recipient       string // case-insensitive substring on to_email
	Limit           int
	CursorCreatedAt time.Time
	CursorID        string
}

// EmailLogRepository is the append-only send-audit surface (CON-154 §7).
type EmailLogRepository interface {
	Insert(ctx context.Context, l *models.EmailLog) error
	// UpdateStatusByProviderMessageID updates the row a delivery webhook refers
	// to (matched on the Resend message id). Returns whether a row matched.
	UpdateStatusByProviderMessageID(ctx context.Context, providerMessageID string, status models.EmailLogStatus) (bool, error)
	// GetByProviderMessageID resolves the log row a delivery webhook refers to
	// (CON-298). Returns (nil, nil) when no row matches — an unknown message id is
	// not an error, so the webhook can ack rather than trigger a retry storm.
	GetByProviderMessageID(ctx context.Context, providerMessageID string) (*models.EmailLog, error)
	// ListByTenant returns a tenant's emails newest-first (created_at desc, id
	// desc), keyset-paginated (CON-298). NULL-tenant (system) rows never match a
	// non-empty tenant, so they're excluded. Returns at most f.Limit rows.
	ListByTenant(ctx context.Context, f EmailListFilter) ([]models.EmailLog, error)
	// GetByIDForTenant fetches one email scoped to the tenant (CON-298). Returns
	// (nil, nil) when the id doesn't exist or belongs to another tenant.
	GetByIDForTenant(ctx context.Context, tenantID, id string) (*models.EmailLog, error)
	// DeleteOlderThan drops rows created before cutoff (retention sweep),
	// mirroring PostLogRepository. Returns the number removed.
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	// ExistsByIdempotencyKey reports whether any log row already carries this
	// idempotency key (CON-219). The connection-expiry sweep calls it before
	// enqueuing so a multi-day expiry window swept many times notifies each
	// (account, stage, expiry, owner) at most once — durable dedupe that outlives
	// the provider's own idempotency-key TTL. The key is globally unique (partial
	// unique index), so no tenant scope is needed.
	ExistsByIdempotencyKey(ctx context.Context, key string) (bool, error)
}

type emailLogRepository struct {
	db *bun.DB
}

// NewEmailLogRepository returns a Bun-backed EmailLogRepository.
func NewEmailLogRepository(db *bun.DB) EmailLogRepository {
	return &emailLogRepository{db: db}
}

func (r *emailLogRepository) Insert(ctx context.Context, l *models.EmailLog) error {
	_, err := r.db.NewInsert().Model(l).Exec(ctx)
	return err
}

func (r *emailLogRepository) UpdateStatusByProviderMessageID(ctx context.Context, providerMessageID string, status models.EmailLogStatus) (bool, error) {
	if providerMessageID == "" {
		return false, nil
	}
	res, err := r.db.NewUpdate().
		Model((*models.EmailLog)(nil)).
		Set("status = ?", status).
		Set("updated_at = now()").
		Where("provider_message_id = ?", providerMessageID).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *emailLogRepository) GetByProviderMessageID(ctx context.Context, providerMessageID string) (*models.EmailLog, error) {
	if providerMessageID == "" {
		return nil, nil
	}
	l := new(models.EmailLog)
	err := r.db.NewSelect().
		Model(l).
		Where("provider_message_id = ?", providerMessageID).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (r *emailLogRepository) ListByTenant(ctx context.Context, f EmailListFilter) ([]models.EmailLog, error) {
	var rows []models.EmailLog
	if f.TenantID == "" {
		return rows, nil
	}
	q := r.db.NewSelect().Model(&rows).Where("tenant_id = ?", f.TenantID)
	if len(f.Statuses) > 0 {
		q = q.Where("status IN (?)", bun.In(f.Statuses))
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Recipient != "" {
		// Treat the input as a literal substring: escape LIKE metacharacters.
		q = q.Where(`to_email ILIKE ? ESCAPE '\'`, "%"+escapeLike(f.Recipient)+"%")
	}
	if f.CursorID != "" {
		// Keyset: strictly older than the cursor in (created_at, id) order.
		q = q.Where("(created_at, id) < (?, ?)", f.CursorCreatedAt, f.CursorID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	err := q.Order("created_at DESC", "id DESC").Limit(limit).Scan(ctx)
	return rows, err
}

func (r *emailLogRepository) GetByIDForTenant(ctx context.Context, tenantID, id string) (*models.EmailLog, error) {
	if tenantID == "" || id == "" {
		return nil, nil
	}
	l := new(models.EmailLog)
	err := r.db.NewSelect().
		Model(l).
		Where("id = ?", id).
		Where("tenant_id = ?", tenantID).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// escapeLike escapes the LIKE/ILIKE metacharacters so a user-supplied substring
// matches literally (escape char '\', see the ESCAPE clause above).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (r *emailLogRepository) ExistsByIdempotencyKey(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	return r.db.NewSelect().
		Model((*models.EmailLog)(nil)).
		Where("idempotency_key = ?", key).
		Exists(ctx)
}

func (r *emailLogRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.NewDelete().
		Model((*models.EmailLog)(nil)).
		Where("created_at < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
