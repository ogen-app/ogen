package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ZernioConnectSessionRepository persists the short-lived headless-connect
// state. It is intentionally NOT tenant-scoped at the query layer:
// the OAuth callback reads a row by id BEFORE a tenant is in context and
// resolves the tenant from it; the authenticated picker handlers then assert
// the row's tenant matches the caller. All point reads exclude expired rows.
type ZernioConnectSessionRepository interface {
	// Create inserts a fresh session. TenantID must be set on the model (the
	// model carries no tenant-scoping hook, so nothing stamps it implicitly).
	Create(ctx context.Context, s *models.ZernioConnectSession) error
	// GetLive returns the row by id when it exists and has not expired as of
	// `now`, or sql.ErrNoRows otherwise. Not tenant-scoped by design.
	GetLive(ctx context.Context, id string, now time.Time) (*models.ZernioConnectSession, error)
	// Update persists status / sealed_secrets / options and bumps updated_at.
	Update(ctx context.Context, s *models.ZernioConnectSession) error
	// Delete removes a session by id (idempotent — unknown id is a no-op).
	Delete(ctx context.Context, id string) error
	// DeleteExpired removes rows whose expires_at is at or before `now`,
	// returning the number swept. Called from the periodic cleanup job.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

type zernioConnectSessionRepository struct {
	db *bun.DB
}

func NewZernioConnectSessionRepository(db *bun.DB) ZernioConnectSessionRepository {
	return &zernioConnectSessionRepository{db: db}
}

func (r *zernioConnectSessionRepository) Create(ctx context.Context, s *models.ZernioConnectSession) error {
	_, err := r.db.NewInsert().Model(s).Exec(ctx)
	return err
}

func (r *zernioConnectSessionRepository) GetLive(ctx context.Context, id string, now time.Time) (*models.ZernioConnectSession, error) {
	s := new(models.ZernioConnectSession)
	if err := r.db.NewSelect().Model(s).
		Where("zcs.id = ?", id).
		Where("zcs.expires_at > ?", now).
		Scan(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *zernioConnectSessionRepository) Update(ctx context.Context, s *models.ZernioConnectSession) error {
	s.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(s).
		Column("status", "sealed_secrets", "options", "updated_at").
		WherePK().
		Exec(ctx)
	return err
}

func (r *zernioConnectSessionRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.NewDelete().Model((*models.ZernioConnectSession)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *zernioConnectSessionRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.NewDelete().Model((*models.ZernioConnectSession)(nil)).
		Where("expires_at <= ?", now).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
