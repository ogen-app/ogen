package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TenantTierVersionRepository reads the immutable tier-version snapshots + their
// price rows (CON-243). Versions are a GLOBAL operator table (like tenant_tiers),
// so — unlike TenantScoped repositories — this one carries no tenantctx and reads
// cross-tenant. Writes (draft authoring, publish, retire) live on the Harbor gRPC
// surface (CON-294); this repository is read-only.
type TenantTierVersionRepository interface {
	GetByID(ctx context.Context, id string) (*models.TenantTierVersion, error)
	// LatestActiveByTier returns the highest-versioned active version of a tier,
	// or sql.ErrNoRows if the tier has none.
	LatestActiveByTier(ctx context.Context, tierID string) (*models.TenantTierVersion, error)
	// ListCurrentPurchasable returns each tier's latest active + purchasable
	// version — the public pricing catalog.
	ListCurrentPurchasable(ctx context.Context) ([]models.TenantTierVersion, error)
	PricesByVersion(ctx context.Context, versionID string) ([]models.TenantTierVersionPrice, error)
	PricesByVersionIDs(ctx context.Context, versionIDs []string) (map[string][]models.TenantTierVersionPrice, error)
}

type tenantTierVersionRepository struct {
	db *bun.DB
}

// NewTenantTierVersionRepository returns a Bun-backed TenantTierVersionRepository.
func NewTenantTierVersionRepository(db *bun.DB) TenantTierVersionRepository {
	return &tenantTierVersionRepository{db: db}
}

func (r *tenantTierVersionRepository) GetByID(ctx context.Context, id string) (*models.TenantTierVersion, error) {
	v := new(models.TenantTierVersion)
	if err := r.db.NewSelect().Model(v).Where("ttv.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return v, nil
}

func (r *tenantTierVersionRepository) LatestActiveByTier(ctx context.Context, tierID string) (*models.TenantTierVersion, error) {
	v := new(models.TenantTierVersion)
	err := r.db.NewSelect().Model(v).
		Where("ttv.tier_id = ?", tierID).
		Where("ttv.status = ?", models.TierVersionStatusActive).
		OrderExpr("ttv.version DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return v, nil
}

func (r *tenantTierVersionRepository) ListCurrentPurchasable(ctx context.Context) ([]models.TenantTierVersion, error) {
	var versions []models.TenantTierVersion
	err := r.db.NewSelect().Model(&versions).
		DistinctOn("ttv.tier_id").
		Where("ttv.status = ?", models.TierVersionStatusActive).
		Where("ttv.purchasable = ?", true).
		OrderExpr("ttv.tier_id ASC, ttv.version DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return versions, nil
}

func (r *tenantTierVersionRepository) PricesByVersion(ctx context.Context, versionID string) ([]models.TenantTierVersionPrice, error) {
	var prices []models.TenantTierVersionPrice
	err := r.db.NewSelect().Model(&prices).
		Where("ttvp.tier_version_id = ?", versionID).
		OrderExpr("currency ASC, billing_interval ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return prices, nil
}

func (r *tenantTierVersionRepository) PricesByVersionIDs(ctx context.Context, versionIDs []string) (map[string][]models.TenantTierVersionPrice, error) {
	out := make(map[string][]models.TenantTierVersionPrice, len(versionIDs))
	if len(versionIDs) == 0 {
		return out, nil
	}
	var prices []models.TenantTierVersionPrice
	err := r.db.NewSelect().Model(&prices).
		Where("ttvp.tier_version_id IN (?)", bun.In(versionIDs)).
		OrderExpr("currency ASC, billing_interval ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range prices {
		out[p.TierVersionID] = append(out[p.TierVersionID], p)
	}
	return out, nil
}
