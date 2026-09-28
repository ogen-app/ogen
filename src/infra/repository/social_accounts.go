package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SocialAccountRepository is the persistence surface for the Zernio
// integration's local account mirror (CON-62). The reconciler uses
// the batch operations under a single transaction (ApplyPlan) so each
// sync tick is atomic.
type SocialAccountRepository interface {
	// ListAll returns every row, including soft-deleted, scoped to a
	// profile. The reconciler needs the soft-deleted rows to detect
	// "previously disconnected, now reappearing" → revive.
	ListAll(ctx context.Context, profileID string) ([]models.SocialAccount, error)

	// ListActive returns only rows where deleted_at IS NULL, scoped to
	// a profile. Used by GET /api/integrations/zernio/accounts.
	ListActive(ctx context.Context, profileID string) ([]models.SocialAccount, error)

	// ListActiveByPlatform returns the active (deleted_at IS NULL) accounts
	// for one Zernio platform slug (e.g. "linkedin") under a profile,
	// oldest-connected first. Backs the CON-150 auto-vs-require account
	// selection: 0 → no account, 1 → auto-select, ≥2 → require a choice.
	ListActiveByPlatform(ctx context.Context, profileID, platform string) ([]models.SocialAccount, error)

	// GetActive returns the single active account with the given id under a
	// profile, or sql.ErrNoRows when it is missing, soft-deleted, or belongs
	// to another profile. Backs validation of an explicit CON-150 selection.
	GetActive(ctx context.Context, profileID, id string) (*models.SocialAccount, error)

	// ListActiveTenantProfiles returns the distinct (tenant_id, profile_id)
	// pairs across EVERY tenant with at least one active connected account.
	// MUST be called under a system context (tenantctx.WithSystem) — the
	// TenantScoped hook otherwise restricts it to a single tenant. The
	// CON-153 follower-refresh sweep uses it to enumerate which tenants and
	// profiles to fetch follower stats for.
	ListActiveTenantProfiles(ctx context.Context) ([]TenantProfile, error)

	// SoftDelete marks the active account with the given id as disconnected
	// (deleted_at = now), scoped to the caller's tenant by the TenantScoped
	// hook and to rows that are still active. Returns true when a row was
	// updated, false when the id was unknown or already soft-deleted — so a
	// repeat disconnect is idempotent. Backs the CON-133 disconnect endpoint.
	SoftDelete(ctx context.Context, id string, now time.Time) (bool, error)

	// ApplyPlan persists the reconciler's three sets in a single
	// transaction. Inserts and revives upsert on the primary key
	// (an existing soft-deleted row is brought back to life by
	// clearing deleted_at and refreshing fields). Soft-deletes set
	// deleted_at to the supplied moment.
	ApplyPlan(ctx context.Context, upserts []models.SocialAccount, softDeleteIDs []string, now time.Time) error

	// UpdateHealth writes a Zernio account-health snapshot onto the active row
	// with the given id (CON-219). Scoped to the caller's tenant by the
	// TenantScoped hook and to rows still active (deleted_at IS NULL), so it is a
	// no-op for an id belonging to another tenant or already disconnected. Only
	// the health columns are touched — the reconciler's mirror fields are left
	// alone.
	UpdateHealth(ctx context.Context, id string, h SocialAccountHealth) error
}

// SocialAccountHealth is the mutable health snapshot UpdateHealth persists,
// projected from Zernio's per-account health (CON-219). Pointer fields carry the
// tri-state (unknown → NULL) that a bare bool can't.
type SocialAccountHealth struct {
	TokenExpiresAt      *time.Time
	TokenValid          *bool
	HealthStatus        string
	NeedsReconnect      *bool
	LastHealthCheckedAt time.Time
}

// TenantProfile pairs a tenant with its Zernio profile, derived from the
// social_accounts that tenant owns. Used by cross-tenant sweeps (CON-153) to
// enumerate which tenants/profiles to act on without a per-tenant settings
// read.
type TenantProfile struct {
	TenantID  string `bun:"tenant_id"`
	ProfileID string `bun:"profile_id"`
}

type socialAccountRepository struct {
	db *bun.DB
}

func NewSocialAccountRepository(db *bun.DB) SocialAccountRepository {
	return &socialAccountRepository{db: db}
}

func (r *socialAccountRepository) ListActiveTenantProfiles(ctx context.Context) ([]TenantProfile, error) {
	var out []TenantProfile
	// System context skips the tenant predicate (scopeQuery), so this spans
	// every tenant. profile_id != '' guards against a half-provisioned row.
	if err := r.db.NewSelect().Model((*models.SocialAccount)(nil)).
		ColumnExpr("DISTINCT sa.tenant_id, sa.profile_id").
		Where("sa.deleted_at IS NULL").
		Where("sa.profile_id != ''").
		OrderExpr("sa.tenant_id").
		Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *socialAccountRepository) ListAll(ctx context.Context, profileID string) ([]models.SocialAccount, error) {
	var out []models.SocialAccount
	if err := r.db.NewSelect().Model(&out).
		Where("sa.profile_id = ?", profileID).
		OrderExpr("connected_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *socialAccountRepository) ListActive(ctx context.Context, profileID string) ([]models.SocialAccount, error) {
	var out []models.SocialAccount
	if err := r.db.NewSelect().Model(&out).
		Where("sa.profile_id = ?", profileID).
		Where("sa.deleted_at IS NULL").
		OrderExpr("connected_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *socialAccountRepository) ListActiveByPlatform(ctx context.Context, profileID, platform string) ([]models.SocialAccount, error) {
	var out []models.SocialAccount
	if err := r.db.NewSelect().Model(&out).
		Where("sa.profile_id = ?", profileID).
		Where("sa.platform = ?", platform).
		Where("sa.deleted_at IS NULL").
		OrderExpr("connected_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *socialAccountRepository) GetActive(ctx context.Context, profileID, id string) (*models.SocialAccount, error) {
	acc := new(models.SocialAccount)
	if err := r.db.NewSelect().Model(acc).
		Where("sa.id = ?", id).
		Where("sa.profile_id = ?", profileID).
		Where("sa.deleted_at IS NULL").
		Scan(ctx); err != nil {
		return nil, err
	}
	return acc, nil
}

func (r *socialAccountRepository) SoftDelete(ctx context.Context, id string, now time.Time) (bool, error) {
	// The TenantScoped BeforeUpdate hook adds `tenant_id = <ctx tenant>`, and
	// `deleted_at IS NULL` makes a repeat call a no-op — together they keep the
	// write both tenant-safe and idempotent. Mirrors the ApplyPlan soft-delete.
	res, err := r.db.NewUpdate().Model((*models.SocialAccount)(nil)).
		Set("deleted_at = ?", now).
		Where("id = ?", id).
		Where("deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *socialAccountRepository) UpdateHealth(ctx context.Context, id string, h SocialAccountHealth) error {
	// The TenantScoped BeforeUpdate hook adds `tenant_id = <ctx tenant>` and
	// `deleted_at IS NULL` keeps the write to still-connected rows — together they
	// make the write tenant-safe and skip a soft-deleted account, mirroring
	// SoftDelete. Only the health columns are set; nil pointers / "" become NULL.
	var status any
	if h.HealthStatus != "" {
		status = h.HealthStatus
	}
	_, err := r.db.NewUpdate().Model((*models.SocialAccount)(nil)).
		Set("token_expires_at = ?", h.TokenExpiresAt).
		Set("token_valid = ?", h.TokenValid).
		Set("health_status = ?", status).
		Set("needs_reconnect = ?", h.NeedsReconnect).
		Set("last_health_checked_at = ?", h.LastHealthCheckedAt).
		Where("id = ?", id).
		Where("deleted_at IS NULL").
		Exec(ctx)
	return err
}

func (r *socialAccountRepository) ApplyPlan(
	ctx context.Context,
	upserts []models.SocialAccount,
	softDeleteIDs []string,
	now time.Time,
) error {
	if len(upserts) == 0 && len(softDeleteIDs) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if len(upserts) > 0 {
			// ON CONFLICT covers both inserts and revives — a row
			// previously soft-deleted is brought back by clearing
			// deleted_at and refreshing the mutable fields.
			if _, err := tx.NewInsert().Model(&upserts).
				On("CONFLICT (id) DO UPDATE").
				Set("platform = EXCLUDED.platform").
				Set("profile_id = EXCLUDED.profile_id").
				Set("username = EXCLUDED.username").
				Set("display_name = EXCLUDED.display_name").
				Set("avatar_url = EXCLUDED.avatar_url").
				Set("is_active = EXCLUDED.is_active").
				Set("raw_json = EXCLUDED.raw_json").
				Set("last_synced_at = EXCLUDED.last_synced_at").
				Set("deleted_at = NULL").
				Exec(ctx); err != nil {
				return err
			}
		}
		if len(softDeleteIDs) > 0 {
			if _, err := tx.NewUpdate().Model((*models.SocialAccount)(nil)).
				Set("deleted_at = ?", now).
				Where("id IN (?)", bun.List(softDeleteIDs)).
				Where("deleted_at IS NULL").
				Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}
