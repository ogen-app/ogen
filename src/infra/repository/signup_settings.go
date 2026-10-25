package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SignupSettingsRepository persists the single platform_signup_settings row
// and answers the per-account workspace cap.
type SignupSettingsRepository interface {
	// Get returns the row, or the seeded defaults when it is missing.
	Get(ctx context.Context) (models.SignupSettings, error)
	// Update rewrites the row and returns it as stored.
	Update(ctx context.Context, s models.SignupSettings) (models.SignupSettings, error)
	// CountOwnedWorkspacesTx counts the live workspaces an account owns,
	// after taking a transaction-scoped lock on the account, so two parallel
	// creates cannot both pass a cap. db must be the caller's transaction.
	CountOwnedWorkspacesTx(ctx context.Context, db bun.IDB, accountID string) (int, error)
}

type signupSettingsRepository struct {
	db *bun.DB
}

// NewSignupSettingsRepository returns a Bun-backed SignupSettingsRepository.
func NewSignupSettingsRepository(db *bun.DB) SignupSettingsRepository {
	return &signupSettingsRepository{db: db}
}

func (r *signupSettingsRepository) Get(ctx context.Context) (models.SignupSettings, error) {
	var s models.SignupSettings
	err := r.db.NewSelect().Model(&s).Where("pss.id = ?", models.SignupSettingsID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return models.DefaultSignupSettings(), nil
	}
	return s, err
}

func (r *signupSettingsRepository) Update(ctx context.Context, s models.SignupSettings) (models.SignupSettings, error) {
	s.ID = models.SignupSettingsID
	_, err := r.db.NewInsert().Model(&s).
		On("CONFLICT (id) DO UPDATE").
		Set("mode = EXCLUDED.mode").
		Set("max_owned_workspaces_per_account = EXCLUDED.max_owned_workspaces_per_account").
		Set("updated_at = EXCLUDED.updated_at").
		Set("updated_by = EXCLUDED.updated_by").
		Returning("*").
		Exec(ctx)
	return s, err
}

func (r *signupSettingsRepository) CountOwnedWorkspacesTx(ctx context.Context, db bun.IDB, accountID string) (int, error) {
	if _, err := db.NewRaw("SELECT pg_advisory_xact_lock(hashtext('owned_workspaces'), hashtext(?))", accountID).Exec(ctx); err != nil {
		return 0, err
	}
	// users is not TenantScoped for this read: the count spans workspaces.
	// Deleted workspaces do not count; suspended ones do. The legacy default
	// workspace is ignored.
	return db.NewSelect().TableExpr("users AS u").
		Join("JOIN tenants AS t ON t.id = u.tenant_id").
		Where("u.account_id = ?", accountID).
		Where("u.role = ?", models.RoleOwner).
		Where("t.status <> ?", models.TenantStatusDeleted).
		Where("t.deleted_at IS NULL").
		Where("t.id <> ?", models.DefaultTenantID).
		Count(ctx)
}
