package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// UsageLimitsRepository persists per-tenant spend caps in the control-plane
// database. One row per tenant (UNIQUE on tenant_id); reads and
// writes are auto-scoped by the TenantUsageLimit TenantScoped hooks.
type UsageLimitsRepository interface {
	// GetByTenant returns the calling tenant's limit row, or (nil, nil) when
	// no row exists so the caller falls back to config defaults.
	GetByTenant(ctx context.Context) (*models.TenantUsageLimit, error)

	// Upsert creates or updates the calling tenant's single limit row. ID is
	// generated when empty; tenant_id is stamped from context.
	Upsert(ctx context.Context, lim *models.TenantUsageLimit) error
}

type usageLimitsRepository struct {
	db *bun.DB
}

// NewUsageLimitsRepository builds a UsageLimitsRepository on the main pool.
func NewUsageLimitsRepository(db *bun.DB) UsageLimitsRepository {
	return &usageLimitsRepository{db: db}
}

func (r *usageLimitsRepository) GetByTenant(ctx context.Context) (*models.TenantUsageLimit, error) {
	lim := new(models.TenantUsageLimit)
	err := r.db.NewSelect().Model(lim).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lim, nil
}

func (r *usageLimitsRepository) Upsert(ctx context.Context, lim *models.TenantUsageLimit) error {
	if lim.ID == "" {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		lim.ID = id
	}
	lim.UpdatedAt = time.Now().UTC()

	_, err := r.db.NewInsert().Model(lim).
		On("CONFLICT (tenant_id) DO UPDATE").
		Set("daily_cap_micros = EXCLUDED.daily_cap_micros").
		Set("monthly_cap_micros = EXCLUDED.monthly_cap_micros").
		Set("mode = EXCLUDED.mode").
		Set("enabled = EXCLUDED.enabled").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}
