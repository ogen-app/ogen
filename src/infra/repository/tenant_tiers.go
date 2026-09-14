package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TenantTierRepository is the persistence for the tier catalog (CON-208).
//
// Tiers are a GLOBAL operator table (like tenants), so — unlike TenantScoped
// repositories — this one carries no tenantctx and reads/writes cross-tenant.
// Create/Update surface the raw Postgres error on a name-uniqueness clash
// (SQLSTATE 23505); Delete surfaces the FK-restrict error (23503) when the tier
// is still assigned to a tenant. The gRPC admin service maps both to status
// codes (mirroring the handler layer's isUniqueViolation).
type TenantTierRepository interface {
	List(ctx context.Context) ([]models.TenantTier, error)
	GetByID(ctx context.Context, id string) (*models.TenantTier, error)
	Create(ctx context.Context, tier *models.TenantTier) error
	// Update replaces name/color/description and bumps updated_at. Returns
	// sql.ErrNoRows if no tier has the given id.
	Update(ctx context.Context, tier *models.TenantTier) error
	// Delete removes a tier. Returns false if no row matched. A tier still
	// referenced by a tenant fails the ON DELETE RESTRICT FK (23503).
	Delete(ctx context.Context, id string) (bool, error)
}

type tenantTierRepository struct {
	db *bun.DB
}

// NewTenantTierRepository returns a Bun-backed TenantTierRepository.
func NewTenantTierRepository(db *bun.DB) TenantTierRepository {
	return &tenantTierRepository{db: db}
}

func (r *tenantTierRepository) List(ctx context.Context) ([]models.TenantTier, error) {
	var tiers []models.TenantTier
	if err := r.db.NewSelect().Model(&tiers).OrderExpr("name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return tiers, nil
}

func (r *tenantTierRepository) GetByID(ctx context.Context, id string) (*models.TenantTier, error) {
	tier := new(models.TenantTier)
	err := r.db.NewSelect().Model(tier).Where("tt.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return tier, nil
}

func (r *tenantTierRepository) Create(ctx context.Context, tier *models.TenantTier) error {
	_, err := r.db.NewInsert().Model(tier).Exec(ctx)
	return err
}

func (r *tenantTierRepository) Update(ctx context.Context, tier *models.TenantTier) error {
	res, err := r.db.NewUpdate().Model((*models.TenantTier)(nil)).
		Set("name = ?", tier.Name).
		Set("color = ?", tier.Color).
		Set("description = ?", tier.Description).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", tier.ID).
		Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *tenantTierRepository) Delete(ctx context.Context, id string) (bool, error) {
	// A tier accrues immutable versions (CON-243), each a tenant_tier_versions row
	// with an ON DELETE RESTRICT FK back to the tier. Leftover DRAFT versions (e.g.
	// authored then abandoned) must not wedge deletion of a tier that has no
	// tenants, so drop them first in the same transaction — their price rows
	// cascade, and the immutability trigger still protects published versions (so
	// a tier with active/retired versions, or tenants, still fails the FK).
	var deleted bool
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*models.TenantTierVersion)(nil)).
			Where("tier_id = ?", id).
			Where("status = ?", "draft").
			Exec(ctx); err != nil {
			return err
		}
		res, err := tx.NewDelete().Model((*models.TenantTier)(nil)).Where("id = ?", id).Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = n > 0
		return nil
	})
	return deleted, err
}
