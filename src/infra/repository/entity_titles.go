package repository

import (
	"context"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
)

// ErrUntitledKind is a kind EntityTitleRepository has no titles for.
var ErrUntitledKind = errors.New("no titles for this kind")

// EntityTitleRepository reads the display titles of the entities a view
// context references, in one round trip per kind.
type EntityTitleRepository interface {
	// Titles returns id → title for the ids of kind that exist in the caller's
	// tenant; unknown, cross-tenant and deleted ids are absent. A kind it can't
	// read returns ErrUntitledKind.
	Titles(ctx context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error)
	// CampaignTitles is Titles confined to one campaign's posts or ideas; any
	// other kind returns ErrUntitledKind.
	CampaignTitles(ctx context.Context, kind viewcontext.EntityKind, campaignID string, ids []string) (map[string]string, error)
}

type entityTitleRepository struct {
	db bun.IDB
}

// NewEntityTitleRepository returns an EntityTitleRepository over db.
func NewEntityTitleRepository(db bun.IDB) EntityTitleRepository {
	return &entityTitleRepository{db: db}
}

// titleSource names, for one kind, the model (for tenant scoping), its id
// column, the title expression, any extra condition and the campaign column,
// when the kind belongs to one campaign.
type titleSource struct {
	model    any
	id       string
	title    string
	where    string
	campaign string
}

// titleSources holds a source for every kind a view context can name.
var titleSources = map[viewcontext.EntityKind]titleSource{
	viewcontext.KindIdea:  {(*models.Idea)(nil), "i.id", "i.title", "", "i.campaign_id"},
	viewcontext.KindAsset: {(*models.Asset)(nil), "a.id", "a.title", "", ""},
	// A post's title is optional; its opening words stand in for it.
	viewcontext.KindPost:     {(*models.Post)(nil), "po.id", "coalesce(nullif(po.title, ''), left(po.content, 80))", "", "po.campaign_id"},
	viewcontext.KindCampaign: {(*models.Campaign)(nil), "c.id", "c.name", "c.deleted_at IS NULL", ""},
	viewcontext.KindVoice:    {(*models.BrandVoice)(nil), "bv.id", "bv.name", "", ""},
	viewcontext.KindAudience: {(*models.BrandAudience)(nil), "ba.id", "ba.name", "", ""},
	// A fact has no title; its statement's opening words stand in for it.
	viewcontext.KindFact:   {(*models.BrandFact)(nil), "bf.id", "left(bf.statement, 80)", "", ""},
	viewcontext.KindSeries: {(*models.Series)(nil), "bs.id", "bs.name", "bs.deleted_at IS NULL", ""},
}

// Titles reads the titles of ids of kind in the caller's tenant.
func (r *entityTitleRepository) Titles(ctx context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error) {
	src, ok := titleSources[kind]
	if !ok {
		return nil, ErrUntitledKind
	}
	return r.titles(ctx, src, ids, "")
}

// CampaignTitles reads the titles of ids of kind in one campaign.
func (r *entityTitleRepository) CampaignTitles(ctx context.Context, kind viewcontext.EntityKind, campaignID string, ids []string) (map[string]string, error) {
	src, ok := titleSources[kind]
	if !ok || src.campaign == "" {
		return nil, ErrUntitledKind
	}
	return r.titles(ctx, src, ids, campaignID)
}

// titles reads src's titles of ids, of campaignID's when it is set.
func (r *entityTitleRepository) titles(ctx context.Context, src titleSource, ids []string, campaignID string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	var rows []struct {
		ID    string `bun:"id"`
		Title string `bun:"title"`
	}
	q := r.db.NewSelect().Model(src.model).
		ColumnExpr(src.id+" AS id").
		ColumnExpr(src.title+" AS title").
		Where(src.id+" IN (?)", bun.List(ids))
	if src.where != "" {
		q = q.Where(src.where)
	}
	if campaignID != "" {
		q = q.Where(src.campaign+" = ?", campaignID)
	}
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.ID] = row.Title
	}
	return out, nil
}
