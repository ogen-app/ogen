package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TenantRepository defines all persistence operations for the Tenant domain.
//
// Tenants are the isolation boundary itself, so this repository is
// intentionally NOT routed through the tenant-scoped query layer (CON-97 §6) —
// it operates on the global tenants table directly.
type TenantRepository interface {
	Create(ctx context.Context, tenant *models.Tenant) error
	GetByID(ctx context.Context, id string) (*models.Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*models.Tenant, error)
	Update(ctx context.Context, tenant *models.Tenant) error
	// SoftDeleteTx marks a workspace deleted (CON-147 PR4) on the provided bun.IDB
	// so it can join the delete handler's transaction (which also, later, enqueues
	// the Zernio teardown). It stamps status='deleted' alongside deleted_at
	// (CON-190) to keep the two in lockstep. Idempotent: a second call while
	// already deleted is a no-op. Passing nil uses the default DB.
	SoftDeleteTx(ctx context.Context, tx bun.IDB, id string, at time.Time) error

	// --- CON-208 operator/admin classification read + write (Harbor gRPC) ---

	// GetByIDWithClassification loads a tenant of ANY status (CON-190: operators
	// inspect suspended/deleted tenants too) and hydrates its Tier and Groups.
	// Returns sql.ErrNoRows only for an unknown id.
	GetByIDWithClassification(ctx context.Context, id string) (*models.Tenant, error)
	// ListWithClassification returns tenants of any status (optionally filtered by
	// tier, group and/or status), each hydrated with Tier + Groups, plus the total
	// match count ignoring the page window.
	ListWithClassification(ctx context.Context, f TenantListFilter) ([]models.Tenant, int, error)
	// SetTier reassigns a non-deleted tenant's required tier. Returns false if no
	// such (live) tenant exists; an unknown tier fails the FK (23503).
	SetTier(ctx context.Context, tenantID, tierID string) (bool, error)

	// --- CON-190 operator/admin lifecycle status (Harbor gRPC) ---

	// SetStatus sets a tenant's lifecycle status + reason (CON-190). It reaches a
	// tenant in ANY status (so it can restore a deleted one) and keeps deleted_at
	// in lockstep: set to `at` for status='deleted', cleared to NULL otherwise.
	// reason is stored as-is (the service clears it for non-suspended targets).
	// Returns false if no such tenant id exists.
	SetStatus(ctx context.Context, tenantID, status, reason string, at time.Time) (bool, error)
	// GetStatus returns a tenant's lifecycle status (CON-190), for background-job
	// active-tenant guards. Returns sql.ErrNoRows for an unknown id.
	GetStatus(ctx context.Context, id string) (string, error)
}

// TenantListFilter narrows and pages ListWithClassification. Empty
// TierID/GroupID/Status means "no filter on that dimension"; Limit defaults to
// 100 and is capped at 500.
type TenantListFilter struct {
	TierID  string
	GroupID string
	Status  string
	Limit   int
	Offset  int
}

type tenantRepository struct {
	db *bun.DB
}

// NewTenantRepository returns a Bun-backed TenantRepository.
func NewTenantRepository(db *bun.DB) TenantRepository {
	return &tenantRepository{db: db}
}

func (r *tenantRepository) Create(ctx context.Context, tenant *models.Tenant) error {
	_, err := r.db.NewInsert().Model(tenant).Exec(ctx)
	return err
}

func (r *tenantRepository) GetByID(ctx context.Context, id string) (*models.Tenant, error) {
	tenant := new(models.Tenant)
	err := r.db.NewSelect().Model(tenant).Where("tn.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return tenant, nil
}

func (r *tenantRepository) GetBySlug(ctx context.Context, slug string) (*models.Tenant, error) {
	tenant := new(models.Tenant)
	err := r.db.NewSelect().Model(tenant).Where("tn.slug = ?", slug).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return tenant, nil
}

func (r *tenantRepository) Update(ctx context.Context, tenant *models.Tenant) error {
	_, err := r.db.NewUpdate().Model(tenant).WherePK().Exec(ctx)
	return err
}

func (r *tenantRepository) SoftDeleteTx(ctx context.Context, tx bun.IDB, id string, at time.Time) error {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	_, err := db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("deleted_at = ?", at).
		Set("status = ?", models.TenantStatusDeleted).
		Set("updated_at = ?", at).
		Where("id = ?", id).
		Where("deleted_at IS NULL").
		Exec(ctx)
	return err
}

func (r *tenantRepository) GetByIDWithClassification(ctx context.Context, id string) (*models.Tenant, error) {
	tenant := new(models.Tenant)
	// Any status: operators inspect (and restore) suspended/deleted tenants too
	// (CON-190). Unknown id → ErrNoRows.
	err := r.db.NewSelect().Model(tenant).
		Where("tn.id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	tenants := []models.Tenant{*tenant}
	if err := r.hydrateClassification(ctx, tenants); err != nil {
		return nil, err
	}
	return &tenants[0], nil
}

func (r *tenantRepository) ListWithClassification(ctx context.Context, f TenantListFilter) ([]models.Tenant, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	offset := max(f.Offset, 0)

	var tenants []models.Tenant
	// Any status by default (CON-190): the operator console lists all tenants and
	// badges them by status; pass f.Status to narrow (e.g. only 'suspended').
	q := r.db.NewSelect().Model(&tenants)
	if f.Status != "" {
		q = q.Where("tn.status = ?", f.Status)
	}
	if f.TierID != "" {
		q = q.Where("tn.tier_id = ?", f.TierID)
	}
	if f.GroupID != "" {
		q = q.Where(
			"EXISTS (SELECT 1 FROM tenant_group_assignments AS tga WHERE tga.tenant_id = tn.id AND tga.group_id = ?)",
			f.GroupID,
		)
	}
	// Tiebreak on the PK so ties on created_at yield a total, stable order —
	// otherwise limit/offset paging could drop or repeat rows across pages. The
	// id tiebreak is pinned to the C (byte) collation so the order is identical
	// across clusters regardless of their default collation: a locale collation
	// sorts mixed-case Sqids case-insensitively (stable per-cluster but not
	// portable across dev/CI/prod, and diverging from a bytewise sort.Strings).
	total, err := q.OrderExpr(`tn.created_at ASC, tn.id COLLATE "C" ASC`).Limit(limit).Offset(offset).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	if err := r.hydrateClassification(ctx, tenants); err != nil {
		return nil, 0, err
	}
	return tenants, total, nil
}

func (r *tenantRepository) SetTier(ctx context.Context, tenantID, tierID string) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("tier_id = ?", tierID).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", tenantID).
		Where("deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *tenantRepository) SetStatus(ctx context.Context, tenantID, status, reason string, at time.Time) (bool, error) {
	// Atomic read-modify-write (CON-190): lock the row, and only write when the
	// status actually changes. Setting a tenant to the status it already has is a
	// success no-op that leaves deleted_at, status_reason and updated_at untouched
	// — so, e.g., a repeated delete keeps the ORIGINAL deletion timestamp rather
	// than resetting it. There is no status/deleted_at predicate on the write, so
	// it reaches a tenant in ANY current state (including 'deleted', for restore).
	// deleted_at is kept in lockstep with status: stamped for 'deleted', cleared
	// otherwise.
	found := false
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var current string
		err := tx.NewSelect().Model((*models.Tenant)(nil)).
			Column("status").
			Where("id = ?", tenantID).
			For("UPDATE").
			Scan(ctx, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // found stays false → distinct not-found result
		}
		if err != nil {
			return err
		}
		found = true
		if current == status {
			return nil // idempotent no-op: no write, timestamps preserved
		}
		q := tx.NewUpdate().Model((*models.Tenant)(nil)).
			Set("status = ?", status).
			Set("status_reason = ?", reason).
			Set("updated_at = ?", at)
		if status == models.TenantStatusDeleted {
			q = q.Set("deleted_at = ?", at)
		} else {
			q = q.Set("deleted_at = NULL")
		}
		_, err = q.Where("id = ?", tenantID).Exec(ctx)
		return err
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

func (r *tenantRepository) GetStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := r.db.NewSelect().Model((*models.Tenant)(nil)).
		Column("status").
		Where("id = ?", id).
		Scan(ctx, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.ErrNoRows
		}
		return "", err
	}
	return status, nil
}

// hydrateClassification loads each tenant's Tier (by tier_id) and Groups (via
// the assignment join) in two batched queries, then attaches them in Go —
// mirroring campaignRepository's hydrate helpers. Groups default to an empty
// (non-nil) slice so the response is [] rather than null.
func (r *tenantRepository) hydrateClassification(ctx context.Context, tenants []models.Tenant) error {
	if len(tenants) == 0 {
		return nil
	}

	// Tiers, deduplicated by id.
	tierIDs := make([]string, 0, len(tenants))
	seen := make(map[string]struct{}, len(tenants))
	for _, t := range tenants {
		if _, ok := seen[t.TierID]; !ok {
			seen[t.TierID] = struct{}{}
			tierIDs = append(tierIDs, t.TierID)
		}
	}
	var tiers []models.TenantTier
	if err := r.db.NewSelect().Model(&tiers).Where("tt.id IN (?)", bun.List(tierIDs)).Scan(ctx); err != nil {
		return err
	}
	tierByID := make(map[string]*models.TenantTier, len(tiers))
	for i := range tiers {
		tierByID[tiers[i].ID] = &tiers[i]
	}

	// Groups per tenant, via the assignment join.
	tenantIDs := make([]string, len(tenants))
	for i, t := range tenants {
		tenantIDs[i] = t.ID
	}
	var rows []struct {
		TenantID    string    `bun:"tenant_id"`
		ID          string    `bun:"id"`
		Name        string    `bun:"name"`
		Color       string    `bun:"color"`
		Description string    `bun:"description"`
		CreatedAt   time.Time `bun:"created_at"`
		UpdatedAt   time.Time `bun:"updated_at"`
	}
	if err := r.db.NewSelect().
		TableExpr("tenant_group_assignments AS tga").
		ColumnExpr("tga.tenant_id, tg.id, tg.name, tg.color, tg.description, tg.created_at, tg.updated_at").
		Join("JOIN tenant_groups AS tg ON tg.id = tga.group_id").
		Where("tga.tenant_id IN (?)", bun.List(tenantIDs)).
		OrderExpr("tg.name ASC").
		Scan(ctx, &rows); err != nil {
		return err
	}
	groupsByTenant := make(map[string][]models.TenantGroup, len(tenants))
	for _, row := range rows {
		groupsByTenant[row.TenantID] = append(groupsByTenant[row.TenantID], models.TenantGroup{
			ID:          row.ID,
			Name:        row.Name,
			Color:       row.Color,
			Description: row.Description,
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   row.UpdatedAt,
		})
	}

	for i := range tenants {
		tenants[i].Tier = tierByID[tenants[i].TierID]
		if g, ok := groupsByTenant[tenants[i].ID]; ok {
			tenants[i].Groups = g
		} else {
			tenants[i].Groups = []models.TenantGroup{}
		}
	}
	return nil
}
