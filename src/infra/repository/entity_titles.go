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
}

type entityTitleRepository struct {
	db bun.IDB
}

// NewEntityTitleRepository returns an EntityTitleRepository over db.
func NewEntityTitleRepository(db bun.IDB) EntityTitleRepository {
	return &entityTitleRepository{db: db}
}

// titleSources names, per kind, the model (for tenant scoping), its id
// column, the title expression and any extra condition.
var titleSources = map[viewcontext.EntityKind]struct {
	model any
	id    string
	title string
	where string
}{
	viewcontext.KindIdea:  {(*models.Idea)(nil), "i.id", "i.title", ""},
	viewcontext.KindAsset: {(*models.Asset)(nil), "a.id", "a.title", ""},
	// A post's title is optional; its opening words stand in for it.
	viewcontext.KindPost:     {(*models.Post)(nil), "po.id", "coalesce(nullif(po.title, ''), left(po.content, 80))", ""},
	viewcontext.KindCampaign: {(*models.Campaign)(nil), "c.id", "c.name", "c.deleted_at IS NULL"},
}

func (r *entityTitleRepository) Titles(ctx context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error) {
	src, ok := titleSources[kind]
	if !ok {
		return nil, ErrUntitledKind
	}
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
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.ID] = row.Title
	}
	return out, nil
}
