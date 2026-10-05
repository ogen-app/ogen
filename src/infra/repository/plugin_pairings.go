package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PluginPairingRepository persists the plugin pairing handshake. It is not
// tenant-scoped: a pairing exists before a workspace is chosen, and both keys
// are unguessable capabilities. Every read excludes rows past expires_at, so
// correctness never depends on the sweep.
type PluginPairingRepository interface {
	Create(ctx context.Context, p *models.PluginPairing) error
	// GetLiveByReadHash / GetLiveByWriteHash return the unexpired pairing for a
	// key hash, or sql.ErrNoRows.
	GetLiveByReadHash(ctx context.Context, hash string, now time.Time) (*models.PluginPairing, error)
	GetLiveByWriteHash(ctx context.Context, hash string, now time.Time) (*models.PluginPairing, error)
	// Approve moves a live pending pairing to approved, recording the tenant,
	// the token and its sealed plaintext. It reports false when the pairing is
	// no longer pending or has expired, so a double approve can't mint twice.
	// db may be a transaction (nil uses the repository's handle).
	Approve(ctx context.Context, db bun.IDB, p *models.PluginPairing, now time.Time) (bool, error)
	// Deny moves a live pending pairing to denied, reporting false otherwise.
	Deny(ctx context.Context, id string, now time.Time) (bool, error)
	// Collect deletes a live approved pairing by read-key hash and returns it,
	// in one statement, so its sealed token is handed out at most once. It
	// returns sql.ErrNoRows when there is nothing to collect.
	Collect(ctx context.Context, readHash string, now time.Time) (*models.PluginPairing, error)
	// DeleteExpired removes rows whose expires_at is at or before now and
	// returns how many it swept.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

type pluginPairingRepository struct {
	db *bun.DB
}

func NewPluginPairingRepository(db *bun.DB) PluginPairingRepository {
	return &pluginPairingRepository{db: db}
}

func (r *pluginPairingRepository) Create(ctx context.Context, p *models.PluginPairing) error {
	_, err := r.db.NewInsert().Model(p).Exec(ctx)
	return err
}

func (r *pluginPairingRepository) GetLiveByReadHash(ctx context.Context, hash string, now time.Time) (*models.PluginPairing, error) {
	return r.getLive(ctx, "read_key_hash", hash, now)
}

func (r *pluginPairingRepository) GetLiveByWriteHash(ctx context.Context, hash string, now time.Time) (*models.PluginPairing, error) {
	return r.getLive(ctx, "write_key_hash", hash, now)
}

func (r *pluginPairingRepository) getLive(ctx context.Context, column, hash string, now time.Time) (*models.PluginPairing, error) {
	p := new(models.PluginPairing)
	if err := r.db.NewSelect().Model(p).
		Where("?TableAlias.? = ?", bun.Ident(column), hash).
		Where("?TableAlias.expires_at > ?", now).
		Scan(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *pluginPairingRepository) Approve(ctx context.Context, db bun.IDB, p *models.PluginPairing, now time.Time) (bool, error) {
	if db == nil {
		db = r.db
	}
	res, err := db.NewUpdate().Model((*models.PluginPairing)(nil)).
		Set("status = ?", models.PluginPairingApproved).
		Set("tenant_id = ?", p.TenantID).
		Set("token_id = ?", p.TokenID).
		Set("sealed_token = ?", p.SealedToken).
		Where("id = ?", p.ID).
		Where("status = ?", models.PluginPairingPending).
		Where("expires_at > ?", now).
		Exec(ctx)
	return affectedOne(res, err)
}

func (r *pluginPairingRepository) Deny(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.PluginPairing)(nil)).
		Set("status = ?", models.PluginPairingDenied).
		Where("id = ?", id).
		Where("status = ?", models.PluginPairingPending).
		Where("expires_at > ?", now).
		Exec(ctx)
	return affectedOne(res, err)
}

func (r *pluginPairingRepository) Collect(ctx context.Context, readHash string, now time.Time) (*models.PluginPairing, error) {
	p := new(models.PluginPairing)
	if err := r.db.NewDelete().Model(p).
		Where("read_key_hash = ?", readHash).
		Where("status = ?", models.PluginPairingApproved).
		Where("expires_at > ?", now).
		Returning("*").
		Scan(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *pluginPairingRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.NewDelete().Model((*models.PluginPairing)(nil)).
		Where("expires_at <= ?", now).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
