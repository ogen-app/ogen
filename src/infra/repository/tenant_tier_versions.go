package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Version write-path sentinels (CON-294). The gRPC PlanAdminService maps these to
// FailedPrecondition; the database immutability trigger is the backstop.
var (
	ErrVersionNotDraft           = errors.New("tier version is not a draft")
	ErrVersionNotActive          = errors.New("tier version is not active")
	ErrVersionHasLiveAssignments = errors.New("tier version has live assignments")
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

	// --- writes: Harbor authoring (CON-294) ---

	// ListByTier returns every version of a tier (all statuses), newest first.
	ListByTier(ctx context.Context, tierID string) ([]models.TenantTierVersion, error)
	// NextVersion returns the next version number for a tier (max + 1, or 1).
	NextVersion(ctx context.Context, tierID string) (int, error)
	// Create inserts a draft version and its price rows in one transaction.
	Create(ctx context.Context, v *models.TenantTierVersion, prices []models.TenantTierVersionPrice) error
	// UpdateDraft replaces a draft version's purchasable + entitlements and its
	// full price set. Returns ErrVersionNotDraft if the version is published,
	// sql.ErrNoRows if it does not exist.
	UpdateDraft(ctx context.Context, v *models.TenantTierVersion, prices []models.TenantTierVersionPrice) error
	// Publish transitions a draft version to active (sets change_reason +
	// published_at). Returns ErrVersionNotDraft otherwise.
	Publish(ctx context.Context, id, changeReason string, at time.Time) error
	// Retire transitions an active version to retired. Returns ErrVersionNotActive
	// otherwise, or ErrVersionHasLiveAssignments if open assignments remain and
	// force is false.
	Retire(ctx context.Context, id string, force bool, at time.Time) error
	// OpenAssignmentCount returns how many tenants hold an open assignment on the
	// version.
	OpenAssignmentCount(ctx context.Context, versionID string) (int, error)
	// OpenAssignmentCounts returns open-assignment counts keyed by version id.
	OpenAssignmentCounts(ctx context.Context, versionIDs []string) (map[string]int, error)
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

func (r *tenantTierVersionRepository) ListByTier(ctx context.Context, tierID string) ([]models.TenantTierVersion, error) {
	var versions []models.TenantTierVersion
	if err := r.db.NewSelect().Model(&versions).
		Where("ttv.tier_id = ?", tierID).
		OrderExpr("ttv.version DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return versions, nil
}

func (r *tenantTierVersionRepository) NextVersion(ctx context.Context, tierID string) (int, error) {
	var maxVersion int
	if err := r.db.NewSelect().Model((*models.TenantTierVersion)(nil)).
		ColumnExpr("COALESCE(MAX(version), 0)").
		Where("tier_id = ?", tierID).
		Scan(ctx, &maxVersion); err != nil {
		return 0, err
	}
	return maxVersion + 1, nil
}

func (r *tenantTierVersionRepository) Create(ctx context.Context, v *models.TenantTierVersion, prices []models.TenantTierVersionPrice) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(v).Exec(ctx); err != nil {
			return err
		}
		return insertPricesTx(ctx, tx, v.ID, prices)
	})
}

func (r *tenantTierVersionRepository) UpdateDraft(ctx context.Context, v *models.TenantTierVersion, prices []models.TenantTierVersionPrice) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		status, err := versionStatusForUpdateTx(ctx, tx, v.ID)
		if err != nil {
			return err
		}
		if status != models.TierVersionStatusDraft {
			return ErrVersionNotDraft
		}
		if _, err := tx.NewUpdate().Model(v).Column("purchasable", "entitlements").WherePK().Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.TenantTierVersionPrice)(nil)).
			Where("tier_version_id = ?", v.ID).Exec(ctx); err != nil {
			return err
		}
		return insertPricesTx(ctx, tx, v.ID, prices)
	})
}

func (r *tenantTierVersionRepository) Publish(ctx context.Context, id, changeReason string, at time.Time) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		status, err := versionStatusForUpdateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if status != models.TierVersionStatusDraft {
			return ErrVersionNotDraft
		}
		_, err = tx.NewUpdate().Model((*models.TenantTierVersion)(nil)).
			Set("status = ?", models.TierVersionStatusActive).
			Set("change_reason = ?", changeReason).
			Set("published_at = ?", at).
			Where("id = ?", id).Exec(ctx)
		return err
	})
}

func (r *tenantTierVersionRepository) Retire(ctx context.Context, id string, force bool, at time.Time) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		status, err := versionStatusForUpdateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if status != models.TierVersionStatusActive {
			return ErrVersionNotActive
		}
		if !force {
			n, err := tx.NewSelect().Model((*models.TenantTierAssignment)(nil)).
				Where("tier_version_id = ?", id).Where("upper_inf(valid)").Count(ctx)
			if err != nil {
				return err
			}
			if n > 0 {
				return ErrVersionHasLiveAssignments
			}
		}
		_, err = tx.NewUpdate().Model((*models.TenantTierVersion)(nil)).
			Set("status = ?", models.TierVersionStatusRetired).
			Set("retired_at = ?", at).
			Where("id = ?", id).Exec(ctx)
		return err
	})
}

func (r *tenantTierVersionRepository) OpenAssignmentCount(ctx context.Context, versionID string) (int, error) {
	return r.db.NewSelect().Model((*models.TenantTierAssignment)(nil)).
		Where("tta.tier_version_id = ?", versionID).
		Where("upper_inf(tta.valid)").
		Count(ctx)
}

func (r *tenantTierVersionRepository) OpenAssignmentCounts(ctx context.Context, versionIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(versionIDs))
	if len(versionIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		TierVersionID string `bun:"tier_version_id"`
		N             int    `bun:"n"`
	}
	err := r.db.NewSelect().Model((*models.TenantTierAssignment)(nil)).
		ColumnExpr("tier_version_id").
		ColumnExpr("count(*) AS n").
		Where("tier_version_id IN (?)", bun.In(versionIDs)).
		Where("upper_inf(valid)").
		GroupExpr("tier_version_id").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.TierVersionID] = row.N
	}
	return out, nil
}

// versionStatusForUpdateTx locks the version row and returns its status
// (sql.ErrNoRows if it does not exist), so a lifecycle transition sees a stable
// status for the rest of the transaction.
func versionStatusForUpdateTx(ctx context.Context, tx bun.Tx, id string) (string, error) {
	var status string
	err := tx.NewSelect().Model((*models.TenantTierVersion)(nil)).
		Column("status").Where("id = ?", id).For("UPDATE").Scan(ctx, &status)
	return status, err
}

// insertPricesTx inserts price rows for a version inside an open transaction,
// stamping the version id on each.
func insertPricesTx(ctx context.Context, tx bun.Tx, versionID string, prices []models.TenantTierVersionPrice) error {
	for i := range prices {
		prices[i].TierVersionID = versionID
		if _, err := tx.NewInsert().Model(&prices[i]).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
