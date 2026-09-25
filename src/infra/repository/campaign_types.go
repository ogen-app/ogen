package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// CampaignTypeRepository defines persistence operations for the CampaignType domain.
// System types (is_system = true) are seeded by migration; only user-created types
// support write operations.
//
// CON-314: a custom type belongs to one workspace. Reads see the system types
// plus the caller's own custom types; writes reach only the caller's own. Any
// other id behaves as missing (sql.ErrNoRows / false), so another workspace's
// ids stay hidden. A system context (no tenant) spans every workspace.
type CampaignTypeRepository interface {
	List(ctx context.Context) ([]models.CampaignType, error)
	GetByID(ctx context.Context, id string) (*models.CampaignType, error)
	GetByIDs(ctx context.Context, ids []string) (map[string]*models.CampaignType, error)
	// NameTaken reports whether name is already used, case-insensitively, by a
	// system type or by one of the caller's own types other than excludeID.
	NameTaken(ctx context.Context, name, excludeID string) (bool, error)
	Create(ctx context.Context, ct *models.CampaignType) error
	// Update writes name, label and description of one of the caller's own
	// types; sql.ErrNoRows when it isn't one.
	Update(ctx context.Context, ct *models.CampaignType) error
	Delete(ctx context.Context, id string) (bool, error)

	AddPhase(ctx context.Context, phase *models.CampaignTypePhase) error
	GetPhaseByID(ctx context.Context, id string) (*models.CampaignTypePhase, error)
	// UpdatePhase writes a phase of one of the caller's own types;
	// sql.ErrNoRows when it isn't one.
	UpdatePhase(ctx context.Context, phase *models.CampaignTypePhase) error
	DeletePhase(ctx context.Context, id string) (bool, error)
}

type campaignTypeRepository struct {
	db *bun.DB
}

func NewCampaignTypeRepository(db *bun.DB) CampaignTypeRepository {
	return &campaignTypeRepository{db: db}
}

// visibleTypes scopes a read to the system types plus the caller's own custom
// types. col is the query's campaigns_types.tenant_id column.
func visibleTypes(ctx context.Context, q *bun.SelectQuery, col string) (*bun.SelectQuery, error) {
	tid, scoped, err := scopeTenantRead(ctx)
	if err != nil {
		return nil, err
	}
	if scoped {
		q = q.Where("(? IS NULL OR ? = ?)", bun.Ident(col), bun.Ident(col), tid)
	}
	return q, nil
}

// ownedTypeIDs is the subquery of the type ids a write may touch: the caller's
// own custom types (every custom type on a system context).
func (r *campaignTypeRepository) ownedTypeIDs(ctx context.Context) (*bun.SelectQuery, error) {
	tid, scoped, err := scopeTenantRead(ctx)
	if err != nil {
		return nil, err
	}
	q := r.db.NewSelect().TableExpr("campaigns_types").Column("id").Where("is_system = FALSE")
	if scoped {
		q = q.Where("tenant_id = ?", tid)
	}
	return q, nil
}

func (r *campaignTypeRepository) List(ctx context.Context) ([]models.CampaignType, error) {
	var types []models.CampaignType
	q, err := visibleTypes(ctx, r.db.NewSelect().Model(&types), "ct.tenant_id")
	if err != nil {
		return nil, err
	}
	if err := q.OrderExpr("ct.name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydratePhases(ctx, types); err != nil {
		return nil, err
	}
	return types, nil
}

func (r *campaignTypeRepository) GetByID(ctx context.Context, id string) (*models.CampaignType, error) {
	ct := new(models.CampaignType)
	q, err := visibleTypes(ctx, r.db.NewSelect().Model(ct).Where("ct.id = ?", id), "ct.tenant_id")
	if err != nil {
		return nil, err
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	types := []models.CampaignType{*ct}
	if err := r.hydratePhases(ctx, types); err != nil {
		return nil, err
	}
	return &types[0], nil
}

func (r *campaignTypeRepository) GetByIDs(ctx context.Context, ids []string) (map[string]*models.CampaignType, error) {
	if len(ids) == 0 {
		return map[string]*models.CampaignType{}, nil
	}
	var types []models.CampaignType
	q, err := visibleTypes(ctx, r.db.NewSelect().Model(&types).Where("ct.id IN (?)", bun.In(ids)), "ct.tenant_id")
	if err != nil {
		return nil, err
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydratePhases(ctx, types); err != nil {
		return nil, err
	}
	byID := make(map[string]*models.CampaignType, len(types))
	for i := range types {
		byID[types[i].ID] = &types[i]
	}
	return byID, nil
}

func (r *campaignTypeRepository) NameTaken(ctx context.Context, name, excludeID string) (bool, error) {
	q, err := visibleTypes(ctx, r.db.NewSelect().
		Model((*models.CampaignType)(nil)).
		Where("lower(ct.name) = lower(?)", name).
		Where("ct.id <> ?", excludeID), "ct.tenant_id")
	if err != nil {
		return false, err
	}
	return q.Exists(ctx)
}

// Create inserts a type; the CampaignType insert hook stamps a custom type's
// owner from the context.
func (r *campaignTypeRepository) Create(ctx context.Context, ct *models.CampaignType) error {
	_, err := r.db.NewInsert().Model(ct).Exec(ctx)
	return err
}

func (r *campaignTypeRepository) Update(ctx context.Context, ct *models.CampaignType) error {
	owned, err := r.ownedTypeIDs(ctx)
	if err != nil {
		return err
	}
	res, err := r.db.NewUpdate().
		Model(ct).
		Column("name", "label", "description", "updated_at").
		WherePK().
		Where("ct.id IN (?)", owned).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *campaignTypeRepository) Delete(ctx context.Context, id string) (bool, error) {
	owned, err := r.ownedTypeIDs(ctx)
	if err != nil {
		return false, err
	}
	res, err := r.db.NewDelete().
		TableExpr("campaigns_types").
		Where("id = ?", id).
		Where("id IN (?)", owned).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *campaignTypeRepository) AddPhase(ctx context.Context, phase *models.CampaignTypePhase) error {
	_, err := r.db.NewInsert().Model(phase).Exec(ctx)
	return err
}

func (r *campaignTypeRepository) GetPhaseByID(ctx context.Context, id string) (*models.CampaignTypePhase, error) {
	phase := new(models.CampaignTypePhase)
	q, err := visibleTypes(ctx, r.db.NewSelect().
		Model(phase).
		Join("JOIN campaigns_types AS ct ON ct.id = ctp.campaign_type_id").
		Where("ctp.id = ?", id), "ct.tenant_id")
	if err != nil {
		return nil, err
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return phase, nil
}

func (r *campaignTypeRepository) UpdatePhase(ctx context.Context, phase *models.CampaignTypePhase) error {
	owned, err := r.ownedTypeIDs(ctx)
	if err != nil {
		return err
	}
	res, err := r.db.NewUpdate().
		Model(phase).
		Column("name", "purpose", "sequence", "updated_at").
		WherePK().
		Where("ctp.campaign_type_id IN (?)", owned).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *campaignTypeRepository) DeletePhase(ctx context.Context, id string) (bool, error) {
	owned, err := r.ownedTypeIDs(ctx)
	if err != nil {
		return false, err
	}
	res, err := r.db.NewDelete().
		TableExpr("campaigns_types_phases").
		Where("id = ?", id).
		Where("campaign_type_id IN (?)", owned).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *campaignTypeRepository) hydratePhases(ctx context.Context, types []models.CampaignType) error {
	for i := range types {
		types[i].Phases = []models.CampaignTypePhase{}
	}
	if len(types) == 0 {
		return nil
	}

	ids := make([]string, len(types))
	for i, ct := range types {
		ids[i] = ct.ID
	}

	var phases []models.CampaignTypePhase
	if err := r.db.NewSelect().
		Model(&phases).
		Where("ctp.campaign_type_id IN (?)", bun.In(ids)).
		OrderExpr("ctp.sequence ASC").
		Scan(ctx); err != nil {
		return err
	}

	index := make(map[string]int, len(types))
	for i, ct := range types {
		index[ct.ID] = i
	}
	for _, p := range phases {
		if i, ok := index[p.CampaignTypeID]; ok {
			types[i].Phases = append(types[i].Phases, p)
		}
	}
	return nil
}
