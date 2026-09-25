package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// IdeaListFilter narrows IdeaRepository.List. The zero value lists every idea in
// the workspace. CampaignID wins over Unfiled when both are set.
type IdeaListFilter struct {
	// CampaignID keeps only ideas attached to this (live) campaign.
	CampaignID string
	// Unfiled keeps only workspace-wide ideas (no live campaign).
	Unfiled bool
}

// IdeaRepository persists the workspace Ideas backlog (CON-315). Every query
// runs through bun's Model API so the TenantScoped hooks scope tenant_id.
//
// Reads resolve campaign_id through a LEFT JOIN on live campaigns: an idea whose
// campaign has since been soft-deleted reads back with a null campaign_id, so
// the UI never links to a campaign that is gone.
type IdeaRepository interface {
	List(ctx context.Context, filter IdeaListFilter) ([]models.Idea, error)
	// GetByID returns sql.ErrNoRows for an unknown or cross-tenant id.
	GetByID(ctx context.Context, id string) (*models.Idea, error)
	Create(ctx context.Context, idea *models.Idea) error
	// Update writes only the named columns plus updated_at, so concurrent edits
	// to different fields don't clobber each other. Returns false when no row
	// matched.
	Update(ctx context.Context, idea *models.Idea, columns ...string) (bool, error)
	Delete(ctx context.Context, id string) (bool, error)
	// LiveCampaignExists reports whether campaignID names a campaign in the
	// caller's tenant that is not soft-deleted (archived counts as live).
	LiveCampaignExists(ctx context.Context, campaignID string) (bool, error)
}

type ideaRepository struct {
	db *bun.DB
}

func NewIdeaRepository(db *bun.DB) IdeaRepository {
	return &ideaRepository{db: db}
}

// selectIdeas builds the shared read: every idea column, with campaign_id taken
// from the joined live campaign rather than the stored FK.
func (r *ideaRepository) selectIdeas(model any) *bun.SelectQuery {
	return r.db.NewSelect().
		Model(model).
		ColumnExpr("i.id, i.title, i.note, i.verdict, i.remind_at, i.decided_at, i.decided_by").
		ColumnExpr("i.created_by, i.created_by_name, i.created_at, i.updated_at").
		ColumnExpr("c.id AS campaign_id").
		Join("LEFT JOIN campaigns AS c ON c.id = i.campaign_id AND c.deleted_at IS NULL")
}

func (r *ideaRepository) List(ctx context.Context, filter IdeaListFilter) ([]models.Idea, error) {
	ideas := []models.Idea{}
	q := r.selectIdeas(&ideas)
	switch {
	case filter.CampaignID != "":
		q = q.Where("c.id = ?", filter.CampaignID)
	case filter.Unfiled:
		q = q.Where("c.id IS NULL")
	}
	if err := q.OrderExpr("i.created_at ASC, i.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return ideas, nil
}

func (r *ideaRepository) GetByID(ctx context.Context, id string) (*models.Idea, error) {
	idea := new(models.Idea)
	if err := r.selectIdeas(idea).Where("i.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return idea, nil
}

func (r *ideaRepository) Create(ctx context.Context, idea *models.Idea) error {
	_, err := r.db.NewInsert().Model(idea).Exec(ctx)
	return err
}

func (r *ideaRepository) Update(ctx context.Context, idea *models.Idea, columns ...string) (bool, error) {
	res, err := r.db.NewUpdate().
		Model(idea).
		Column(append(columns, "updated_at")...).
		WherePK().
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *ideaRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().
		Model((*models.Idea)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// LiveCampaignExists deliberately uses Scan, not Exists: bun's Exists skips the
// BeforeSelect hook, so TenantScoped would not add the tenant predicate and a
// campaign from another tenant would pass.
func (r *ideaRepository) LiveCampaignExists(ctx context.Context, campaignID string) (bool, error) {
	var id string
	err := r.db.NewSelect().
		Model((*models.Campaign)(nil)).
		Column("c.id").
		Where("c.id = ?", campaignID).
		Where("c.deleted_at IS NULL").
		Limit(1).
		Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
