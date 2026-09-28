package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TenantGroupRepository is the persistence for the group catalog and the
// tenant↔group membership join.
//
// Groups are a GLOBAL operator table (like tenants), read/written cross-tenant
// with no tenantctx. Create/Update surface a name-uniqueness clash (23505);
// AddTenant surfaces an FK violation (23503) when the tenant or group is
// missing. The gRPC admin service maps these to status codes.
type TenantGroupRepository interface {
	List(ctx context.Context) ([]models.TenantGroup, error)
	GetByID(ctx context.Context, id string) (*models.TenantGroup, error)
	// ListByTenant returns the groups a tenant belongs to, ordered by name.
	ListByTenant(ctx context.Context, tenantID string) ([]models.TenantGroup, error)
	Create(ctx context.Context, group *models.TenantGroup) error
	// Update replaces name/color/description and bumps updated_at. Returns
	// sql.ErrNoRows if no group has the given id.
	Update(ctx context.Context, group *models.TenantGroup) error
	// Delete removes a group; its assignments cascade. Returns false if no row
	// matched.
	Delete(ctx context.Context, id string) (bool, error)
	// AddTenant assigns a tenant to a group. Idempotent: a repeat is a no-op
	// (ON CONFLICT DO NOTHING). An unknown tenant/group fails the FK (23503).
	AddTenant(ctx context.Context, tenantID, groupID string) error
	// RemoveTenant unassigns a tenant from a group. Returns false if the tenant
	// was not a member (idempotent).
	RemoveTenant(ctx context.Context, tenantID, groupID string) (bool, error)
}

type tenantGroupRepository struct {
	db *bun.DB
}

// NewTenantGroupRepository returns a Bun-backed TenantGroupRepository.
func NewTenantGroupRepository(db *bun.DB) TenantGroupRepository {
	return &tenantGroupRepository{db: db}
}

func (r *tenantGroupRepository) List(ctx context.Context) ([]models.TenantGroup, error) {
	var groups []models.TenantGroup
	if err := r.db.NewSelect().Model(&groups).OrderExpr("name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return groups, nil
}

func (r *tenantGroupRepository) GetByID(ctx context.Context, id string) (*models.TenantGroup, error) {
	group := new(models.TenantGroup)
	err := r.db.NewSelect().Model(group).Where("tg.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return group, nil
}

func (r *tenantGroupRepository) ListByTenant(ctx context.Context, tenantID string) ([]models.TenantGroup, error) {
	var groups []models.TenantGroup
	err := r.db.NewSelect().Model(&groups).
		Join("JOIN tenant_group_assignments AS tga ON tga.group_id = tg.id").
		Where("tga.tenant_id = ?", tenantID).
		OrderExpr("tg.name ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return groups, nil
}

func (r *tenantGroupRepository) Create(ctx context.Context, group *models.TenantGroup) error {
	_, err := r.db.NewInsert().Model(group).Exec(ctx)
	return err
}

func (r *tenantGroupRepository) Update(ctx context.Context, group *models.TenantGroup) error {
	res, err := r.db.NewUpdate().Model((*models.TenantGroup)(nil)).
		Set("name = ?", group.Name).
		Set("color = ?", group.Color).
		Set("description = ?", group.Description).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", group.ID).
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

func (r *tenantGroupRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.TenantGroup)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *tenantGroupRepository) AddTenant(ctx context.Context, tenantID, groupID string) error {
	assignment := &models.TenantGroupAssignment{
		TenantID:  tenantID,
		GroupID:   groupID,
		CreatedAt: time.Now().UTC(),
	}
	_, err := r.db.NewInsert().Model(assignment).
		On("CONFLICT (tenant_id, group_id) DO NOTHING").
		Exec(ctx)
	return err
}

func (r *tenantGroupRepository) RemoveTenant(ctx context.Context, tenantID, groupID string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.TenantGroupAssignment)(nil)).
		Where("tenant_id = ?", tenantID).
		Where("group_id = ?", groupID).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
