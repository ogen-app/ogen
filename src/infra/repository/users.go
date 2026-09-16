package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrLastOwner is returned by the role-change / removal operations when they
// would leave a workspace with no owner. Handlers map it to 409 — a workspace
// must always keep at least one owner (CON-26 §7).
var ErrLastOwner = errors.New("cannot remove or demote the last owner")

// UserRepository defines all persistence operations for the User domain.
type UserRepository interface {
	List(ctx context.Context) ([]models.User, error)
	// CountInTenant counts the tenant's members — the team_seats quota (CON-295).
	CountInTenant(ctx context.Context) (int64, error)
	Create(ctx context.Context, user *models.User) error
	// CreateTx inserts a user on the provided bun.IDB so it can join an outer
	// transaction (e.g. invitation accept creates the user + session atomically).
	// Passing nil falls back to the repository's default DB.
	CreateTx(ctx context.Context, tx bun.IDB, user *models.User) error
	GetByID(ctx context.Context, id string) (*models.User, error)
	// GetByIDWithTenant is GetByID with the Tenant relation eager-loaded, for
	// GET /api/current_user (CON-97).
	GetByIDWithTenant(ctx context.Context, id string) (*models.User, error)
	GetByEmail(ctx context.Context, email string) (*models.User, error)
	// GetByAccountID resolves the membership a login lands on. In CON-147 PR1 an
	// account has exactly one membership, so this returns it; when an account can
	// hold several (PR2+) it returns the earliest-joined as the default. Runs
	// unscoped — the auth path has no tenant yet. sql.ErrNoRows if the account has
	// no membership at all.
	GetByAccountID(ctx context.Context, accountID string) (*models.User, error)
	// GetMembership resolves the membership of accountID in a specific workspace,
	// which is how the auth middleware validates an X-Workspace-Id header before
	// scoping a request to it (CON-147 PR2). Runs unscoped — it IS the check that
	// decides the tenant. sql.ErrNoRows when the account isn't a member there.
	GetMembership(ctx context.Context, accountID, tenantID string) (*models.User, error)
	Update(ctx context.Context, user *models.User) error
	Delete(ctx context.Context, id string) (bool, error)
	// SetRoleGuarded sets a member's role within tenantID, enforcing the
	// >=1-owner invariant in one transaction (it locks the tenant's owner rows so
	// concurrent demotions can't race the workspace to zero owners). Returns the
	// updated user, sql.ErrNoRows if the user isn't in the tenant, or ErrLastOwner
	// when demoting the sole owner (CON-26 §7).
	SetRoleGuarded(ctx context.Context, id, tenantID, newRole string) (*models.User, error)
	// RemoveMemberGuarded deletes a member within tenantID under the same
	// >=1-owner invariant. Returns sql.ErrNoRows if the user isn't in the tenant,
	// or ErrLastOwner when removing the sole owner (CON-26 §7).
	RemoveMemberGuarded(ctx context.Context, id, tenantID string) error

	// ListOwnersByTenant returns every owner (role = owner) membership in
	// tenantID, oldest-joined first (CON-219). Takes the tenant id explicitly
	// because User is not TenantScoped — the caller is a cross-tenant background
	// sweep that names each tenant in turn. Backs the connection-expiry
	// notification's recipient resolution. Never returns sql.ErrNoRows (an empty
	// slice for an unknown tenant); the >=1-owner invariant means a live tenant
	// always has at least one.
	ListOwnersByTenant(ctx context.Context, tenantID string) ([]models.User, error)
}

type userRepository struct {
	db *bun.DB
}

// NewUserRepository returns a Bun-backed UserRepository.
func NewUserRepository(db *bun.DB) UserRepository {
	return &userRepository{db: db}
}

func (r *userRepository) List(ctx context.Context) ([]models.User, error) {
	// Users are not TenantScoped (the auth path looks them up before a tenant is
	// known), so the tenant filter is applied by hand here — otherwise List would
	// return every tenant's users (CON-97).
	tid, scoped, err := scopeTenantRead(ctx)
	if err != nil {
		return nil, err
	}
	var users []models.User
	q := r.db.NewSelect().Model(&users).OrderExpr("created_at ASC")
	if scoped {
		q = q.Where("u.tenant_id = ?", tid)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return users, nil
}

func (r *userRepository) CountInTenant(ctx context.Context) (int64, error) {
	tid, scoped, err := scopeTenantRead(ctx)
	if err != nil {
		return 0, err
	}
	q := r.db.NewSelect().Model((*models.User)(nil))
	if scoped {
		q = q.Where("u.tenant_id = ?", tid)
	}
	n, err := q.Count(ctx)
	return int64(n), err
}

func (r *userRepository) Create(ctx context.Context, user *models.User) error {
	return r.CreateTx(ctx, nil, user)
}

func (r *userRepository) CreateTx(ctx context.Context, tx bun.IDB, user *models.User) error {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	_, err := db.NewInsert().Model(user).Exec(ctx)
	return err
}

func (r *userRepository) GetByID(ctx context.Context, id string) (*models.User, error) {
	// Tenant-scoped by hand (User is not TenantScoped) so one tenant can't read
	// another tenant's user by id — e.g. GET /api/users/:id (CON-97).
	tid, scoped, err := scopeTenantRead(ctx)
	if err != nil {
		return nil, err
	}
	user := new(models.User)
	q := r.db.NewSelect().Model(user).Where("u.id = ?", id)
	if scoped {
		q = q.Where("u.tenant_id = ?", tid)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return user, nil
}

func (r *userRepository) GetByIDWithTenant(ctx context.Context, id string) (*models.User, error) {
	tid, scoped, err := scopeTenantRead(ctx)
	if err != nil {
		return nil, err
	}
	user := new(models.User)
	q := r.db.NewSelect().Model(user).Relation("Tenant").Where("u.id = ?", id)
	if scoped {
		q = q.Where("u.tenant_id = ?", tid)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return user, nil
}

func (r *userRepository) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	user := new(models.User)
	err := r.db.NewSelect().Model(user).Where("u.email = ?", email).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return user, nil
}

func (r *userRepository) GetByAccountID(ctx context.Context, accountID string) (*models.User, error) {
	// Unscoped: the login/auth path resolves the membership before any tenant is
	// known. Joins tenants to skip non-active workspaces (status='active' covers
	// both suspended and soft-deleted — CON-190, extending CON-147 PR4), so an
	// account never lands on a suspended or deleted one; ordered so it resolves to
	// a stable default (the oldest active membership). u.id breaks ties on
	// identical created_at (e.g. memberships seeded in one transaction) so the
	// default is deterministic across calls, not left to the scan order.
	user := new(models.User)
	err := r.db.NewSelect().Model(user).
		Join("JOIN tenants AS t ON t.id = u.tenant_id").
		Where("u.account_id = ?", accountID).
		Where("t.status = ?", models.TenantStatusActive).
		OrderExpr("u.created_at ASC, u.id ASC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return user, nil
}

func (r *userRepository) GetMembership(ctx context.Context, accountID, tenantID string) (*models.User, error) {
	// Unscoped by design: this lookup is what authorises scoping a request to
	// tenantID, so it can't itself depend on a tenant already being in context.
	// Joins tenants and requires status='active' so a suspended or soft-deleted
	// workspace resolves to ErrNoRows — indistinguishable from one the account was
	// never in, which is what makes a non-active workspace unreachable (CON-190,
	// extending CON-147 PR4). This single predicate is the consequence chokepoint:
	// RequireAuth re-resolves membership on every request, so suspending a tenant
	// locks its users out on their next request without touching their session.
	user := new(models.User)
	err := r.db.NewSelect().Model(user).
		Join("JOIN tenants AS t ON t.id = u.tenant_id").
		Where("u.account_id = ?", accountID).
		Where("u.tenant_id = ?", tenantID).
		Where("t.status = ?", models.TenantStatusActive).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return user, nil
}

func (r *userRepository) ListOwnersByTenant(ctx context.Context, tenantID string) ([]models.User, error) {
	// User is not TenantScoped, so the tenant filter is applied by hand (like
	// List). Ordered oldest-first + id tiebreak so the recipient set is
	// deterministic across ticks.
	var owners []models.User
	if err := r.db.NewSelect().Model(&owners).
		Where("u.tenant_id = ?", tenantID).
		Where("u.role = ?", models.RoleOwner).
		OrderExpr("u.created_at ASC, u.id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return owners, nil
}

func (r *userRepository) Update(ctx context.Context, user *models.User) error {
	_, err := r.db.NewUpdate().Model(user).WherePK().Exec(ctx)
	return err
}

func (r *userRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.User)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *userRepository) SetRoleGuarded(ctx context.Context, id, tenantID, newRole string) (*models.User, error) {
	var updated *models.User
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		target := new(models.User)
		if err := tx.NewSelect().Model(target).
			Where("id = ?", id).Where("tenant_id = ?", tenantID).
			For("UPDATE").Scan(ctx); err != nil {
			return err // sql.ErrNoRows => not in this tenant
		}
		if target.Role == models.RoleOwner && newRole != models.RoleOwner {
			if err := ensureNotLastOwnerTx(ctx, tx, tenantID); err != nil {
				return err
			}
		}
		target.Role = newRole
		target.UpdatedAt = time.Now().UTC()
		if _, err := tx.NewUpdate().Model(target).Column("role", "updated_at").WherePK().Exec(ctx); err != nil {
			return err
		}
		updated = target
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *userRepository) RemoveMemberGuarded(ctx context.Context, id, tenantID string) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		target := new(models.User)
		if err := tx.NewSelect().Model(target).
			Where("id = ?", id).Where("tenant_id = ?", tenantID).
			For("UPDATE").Scan(ctx); err != nil {
			return err // sql.ErrNoRows => not in this tenant
		}
		if target.Role == models.RoleOwner {
			if err := ensureNotLastOwnerTx(ctx, tx, tenantID); err != nil {
				return err
			}
		}
		_, err := tx.NewDelete().Model((*models.User)(nil)).
			Where("id = ?", id).Where("tenant_id = ?", tenantID).Exec(ctx)
		return err
	})
}

// ensureNotLastOwnerTx returns ErrLastOwner unless tenantID would still have an
// owner after the caller demotes or removes one. It locks the tenant's owner
// rows (FOR UPDATE) so concurrent demotions/removals serialize and can't race
// the workspace down to zero owners (CON-26 §7). Must be called inside a tx.
func ensureNotLastOwnerTx(ctx context.Context, tx bun.Tx, tenantID string) error {
	var owners []models.User
	if err := tx.NewSelect().Model(&owners).Column("id").
		Where("tenant_id = ?", tenantID).
		Where("role = ?", models.RoleOwner).
		For("UPDATE").
		Scan(ctx); err != nil {
		return err
	}
	if len(owners) <= 1 {
		return ErrLastOwner
	}
	return nil
}
