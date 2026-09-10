package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PlatformGlobalLimitsRepository persists the single-row cross-platform safety
// ceilings (CON-292). The row always exists (seeded by migration); Get returns
// sql.ErrNoRows only on a fresh/broken DB, which callers treat as "fall back to
// the built-in defaults".
type PlatformGlobalLimitsRepository interface {
	Get(ctx context.Context) (*models.PlatformGlobalLimits, error)
	Update(ctx context.Context, limits *models.PlatformGlobalLimits) error
}

type platformGlobalLimitsRepository struct {
	db *bun.DB
}

// NewPlatformGlobalLimitsRepository returns a Bun-backed repository.
func NewPlatformGlobalLimitsRepository(db *bun.DB) PlatformGlobalLimitsRepository {
	return &platformGlobalLimitsRepository{db: db}
}

func (r *platformGlobalLimitsRepository) Get(ctx context.Context) (*models.PlatformGlobalLimits, error) {
	limits := new(models.PlatformGlobalLimits)
	err := r.db.NewSelect().Model(limits).Where("pgl.id = ?", models.PlatformGlobalLimitsID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return limits, nil
}

// Update rewrites the single row. The id and updated_at are server-owned so a
// caller can't move the row off 'global' or forge the timestamp.
func (r *platformGlobalLimitsRepository) Update(ctx context.Context, limits *models.PlatformGlobalLimits) error {
	limits.ID = models.PlatformGlobalLimitsID
	limits.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(limits).WherePK().Exec(ctx)
	return err
}
