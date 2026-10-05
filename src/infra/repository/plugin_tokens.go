package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PluginTokenTouchInterval throttles last_used_at writes: a token used
// continuously is stamped at most once per interval.
const PluginTokenTouchInterval = 10 * time.Minute

// PluginTokenRepository persists plugin credentials. PluginToken carries a
// plain tenant_id (the auth middleware finds a token before any tenant is
// known), so every method past the hash lookup takes the tenant explicitly.
type PluginTokenRepository interface {
	// Create inserts a token. db may be a transaction (nil uses the
	// repository's handle).
	Create(ctx context.Context, db bun.IDB, t *models.PluginToken) error
	// GetActiveByHash returns the unrevoked token with this hash, or
	// sql.ErrNoRows. Unscoped by design: it is how the tenant is found.
	GetActiveByHash(ctx context.Context, hash string) (*models.PluginToken, error)
	// GetActive returns tenantID's unrevoked token id, or sql.ErrNoRows.
	GetActive(ctx context.Context, tenantID, id string) (*models.PluginToken, error)
	// ListActive returns tenantID's unrevoked tokens with their member's name
	// and email, newest first. A non-empty userID narrows it to that member.
	ListActive(ctx context.Context, tenantID, userID string) ([]models.PluginConnection, error)
	// Revoke stamps revoked_at on tenantID's token id, reporting false when it
	// is unknown or already revoked.
	Revoke(ctx context.Context, tenantID, id string, now time.Time) (bool, error)
	// TouchLastUsed stamps last_used_at unless it was stamped within
	// PluginTokenTouchInterval of now.
	TouchLastUsed(ctx context.Context, id string, now time.Time) error
}

type pluginTokenRepository struct {
	db *bun.DB
}

func NewPluginTokenRepository(db *bun.DB) PluginTokenRepository {
	return &pluginTokenRepository{db: db}
}

func (r *pluginTokenRepository) Create(ctx context.Context, db bun.IDB, t *models.PluginToken) error {
	if db == nil {
		db = r.db
	}
	_, err := db.NewInsert().Model(t).Exec(ctx)
	return err
}

func (r *pluginTokenRepository) GetActiveByHash(ctx context.Context, hash string) (*models.PluginToken, error) {
	t := new(models.PluginToken)
	if err := r.db.NewSelect().Model(t).
		Where("pt.token_hash = ?", hash).
		Where("pt.revoked_at IS NULL").
		Scan(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *pluginTokenRepository) GetActive(ctx context.Context, tenantID, id string) (*models.PluginToken, error) {
	t := new(models.PluginToken)
	if err := r.db.NewSelect().Model(t).
		Where("pt.id = ?", id).
		Where("pt.tenant_id = ?", tenantID).
		Where("pt.revoked_at IS NULL").
		Scan(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *pluginTokenRepository) ListActive(ctx context.Context, tenantID, userID string) ([]models.PluginConnection, error) {
	var out []models.PluginConnection
	q := r.db.NewSelect().Model(&out).
		ColumnExpr("pt.*").
		ColumnExpr("u.name AS user_name, u.email AS user_email").
		Join("JOIN users AS u ON u.id = pt.user_id").
		Where("pt.tenant_id = ?", tenantID).
		Where("pt.revoked_at IS NULL").
		OrderExpr("pt.created_at DESC, pt.id DESC")
	if userID != "" {
		q = q.Where("pt.user_id = ?", userID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	if out == nil {
		out = []models.PluginConnection{}
	}
	return out, nil
}

func (r *pluginTokenRepository) Revoke(ctx context.Context, tenantID, id string, now time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.PluginToken)(nil)).
		Set("revoked_at = ?", now).
		Where("id = ?", id).
		Where("tenant_id = ?", tenantID).
		Where("revoked_at IS NULL").
		Exec(ctx)
	return affectedOne(res, err)
}

func (r *pluginTokenRepository) TouchLastUsed(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.NewUpdate().Model((*models.PluginToken)(nil)).
		Set("last_used_at = ?", now).
		Where("id = ?", id).
		Where("last_used_at IS NULL OR last_used_at <= ?", now.Add(-PluginTokenTouchInterval)).
		Exec(ctx)
	return err
}

// affectedOne turns a guarded single-row write into whether it matched.
func affectedOne(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
