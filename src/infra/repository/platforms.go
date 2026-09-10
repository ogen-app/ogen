package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PlatformRepository defines all persistence operations for the Platform domain.
type PlatformRepository interface {
	// List returns every platform (enabled and disabled), ordered by
	// sort_order then created_at. The DB-sourced resolver and the operator
	// ListPlatforms both need disabled rows, so filtering is the caller's job.
	List(ctx context.Context) ([]models.Platform, error)
	// ListEnabled returns only enabled platforms in the same order — the
	// composer-facing GET /api/platforms view (CON-292 §7).
	ListEnabled(ctx context.Context) ([]models.Platform, error)
	Create(ctx context.Context, platform *models.Platform) error
	GetByID(ctx context.Context, id string) (*models.Platform, error)
	// GetByZernioID resolves a platform by its Zernio wire slug (CON-292).
	GetByZernioID(ctx context.Context, zernioID string) (*models.Platform, error)
	Update(ctx context.Context, platform *models.Platform) error
	// SetEnabled flips the soft on/off switch and returns the updated row.
	SetEnabled(ctx context.Context, id string, enabled bool) (*models.Platform, error)
	// InUseCounts reports how many connected accounts and scheduled (not-yet-
	// published) posts reference the platform — the numbers the operator
	// disable/delete guard reports (CON-292 §6 PlatformUsage / §11).
	InUseCounts(ctx context.Context, p *models.Platform) (accounts, scheduledPosts int, err error)
	Delete(ctx context.Context, id string) (bool, error)
}

type platformRepository struct {
	db *bun.DB
}

// NewPlatformRepository returns a Bun-backed PlatformRepository.
func NewPlatformRepository(db *bun.DB) PlatformRepository {
	return &platformRepository{db: db}
}

func (r *platformRepository) List(ctx context.Context) ([]models.Platform, error) {
	var platforms []models.Platform
	if err := r.db.NewSelect().Model(&platforms).OrderExpr("sort_order ASC, created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return platforms, nil
}

func (r *platformRepository) ListEnabled(ctx context.Context) ([]models.Platform, error) {
	var platforms []models.Platform
	if err := r.db.NewSelect().Model(&platforms).
		Where("enabled = ?", true).
		OrderExpr("sort_order ASC, created_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return platforms, nil
}

func (r *platformRepository) Create(ctx context.Context, platform *models.Platform) error {
	_, err := r.db.NewInsert().Model(platform).Exec(ctx)
	return err
}

func (r *platformRepository) GetByID(ctx context.Context, id string) (*models.Platform, error) {
	platform := new(models.Platform)
	err := r.db.NewSelect().Model(platform).Where("pl.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return platform, nil
}

func (r *platformRepository) GetByZernioID(ctx context.Context, zernioID string) (*models.Platform, error) {
	platform := new(models.Platform)
	err := r.db.NewSelect().Model(platform).Where("pl.zernio_id = ?", zernioID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return platform, nil
}

func (r *platformRepository) Update(ctx context.Context, platform *models.Platform) error {
	_, err := r.db.NewUpdate().Model(platform).WherePK().Exec(ctx)
	return err
}

func (r *platformRepository) SetEnabled(ctx context.Context, id string, enabled bool) (*models.Platform, error) {
	res, err := r.db.NewUpdate().
		Model((*models.Platform)(nil)).
		Set("enabled = ?", enabled).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return r.GetByID(ctx, id)
}

func (r *platformRepository) InUseCounts(ctx context.Context, p *models.Platform) (accounts, scheduledPosts int, err error) {
	// Connected accounts join on the Zernio slug (social_accounts.platform holds
	// the wire slug, e.g. "twitter"); soft-deleted accounts don't count.
	if p.ZernioID != "" {
		accounts, err = r.db.NewSelect().Model((*models.SocialAccount)(nil)).
			Where("platform = ?", p.ZernioID).
			Where("deleted_at IS NULL").
			Count(ctx)
		if err != nil {
			return 0, 0, err
		}
	}
	// Scheduled posts join on the Sqid (posts.platform_id) and are the ones that
	// would still fire after a disable — the in-flight work §11 preserves.
	scheduledPosts, err = r.db.NewSelect().Model((*models.Post)(nil)).
		Where("platform_id = ?", p.ID).
		Where("status IN (?)", bun.In([]string{
			string(models.PostStatusScheduled),
			string(models.PostStatusScheduledForManualPublish),
		})).
		Count(ctx)
	if err != nil {
		return 0, 0, err
	}
	return accounts, scheduledPosts, nil
}

func (r *platformRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.Platform)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
