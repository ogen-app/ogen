package repository

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// IdeaMergeSet is what a merge decides on: the ideas it names that exist in
// the workspace, oldest first, and the ids among them a post was made from.
type IdeaMergeSet struct {
	Ideas    []models.Idea
	Compiled []string
}

// IdeaMerge is a planned merge: Keep is written with Columns (updated_at
// is added), then the ideas of Delete are deleted.
type IdeaMerge struct {
	Keep    *models.Idea
	Columns []string
	Delete  []string
}

func (r *ideaRepository) OpenIdeas(ctx context.Context, campaignID string, limit int) ([]models.Idea, bool, error) {
	ideas := []models.Idea{}
	q := r.selectIdeas(&ideas).
		Where("i.verdict IS DISTINCT FROM ?", models.IdeaVerdictNo).
		Where("NOT EXISTS (SELECT 1 FROM post_ideas AS pi WHERE pi.idea_id = i.id AND pi.tenant_id = i.tenant_id)").
		OrderExpr("i.created_at DESC, i.id DESC").
		Limit(limit + 1)
	if campaignID != "" {
		q = q.Where("c.id = ?", campaignID)
	} else {
		q = q.Where("c.id IS NULL")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, false, err
	}
	more := len(ideas) > limit
	return ideas[:min(len(ideas), limit)], more, nil
}

func (r *ideaRepository) MergeSet(ctx context.Context, ids []string) (IdeaMergeSet, error) {
	return mergeSetOn(ctx, r.db, ids, false)
}

func (r *ideaRepository) MergeIdeas(ctx context.Context, ids []string, plan func(IdeaMergeSet) (IdeaMerge, error)) (*models.Idea, error) {
	kept := []models.Idea{}
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		set, err := mergeSetOn(ctx, tx, ids, true)
		if err != nil {
			return err
		}
		m, err := plan(set)
		if err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(m.Keep).Column(append(m.Columns, "updated_at")...).WherePK().Exec(ctx); err != nil {
			return err
		}
		if len(m.Delete) > 0 {
			if _, err := tx.NewDelete().Model((*models.Idea)(nil)).Where("id IN (?)", bun.List(m.Delete)).Exec(ctx); err != nil {
				return err
			}
		}
		// Read back in the transaction, so a failed read undoes the merge
		// rather than reporting an error for one that happened.
		if err := selectIdeasOn(tx, &kept).Where("i.id = ?", m.Keep.ID).Scan(ctx); err != nil {
			return err
		}
		if len(kept) == 0 {
			return sql.ErrNoRows
		}
		return hydrateOn(ctx, tx, kept)
	})
	if err != nil {
		return nil, err
	}
	return &kept[0], nil
}

// mergeSetOn reads a merge set on db. Locked takes the ideas' rows FOR
// UPDATE, which also holds off a post linking to one of them until the
// merge commits.
func mergeSetOn(ctx context.Context, db bun.IDB, ids []string, locked bool) (IdeaMergeSet, error) {
	set := IdeaMergeSet{Ideas: []models.Idea{}, Compiled: []string{}}
	if len(ids) == 0 {
		return set, nil
	}
	q := selectIdeasOn(db, &set.Ideas).Where("i.id IN (?)", bun.List(ids)).OrderExpr("i.created_at, i.id")
	if locked {
		q = q.For("UPDATE OF i")
	}
	if err := q.Scan(ctx); err != nil {
		return set, err
	}
	for i := range set.Ideas {
		set.Ideas[i].DeriveAuthor()
	}
	err := db.NewSelect().Model((*models.PostIdea)(nil)).
		ColumnExpr("DISTINCT pi.idea_id").
		Where("pi.idea_id IN (?)", bun.List(ids)).
		Scan(ctx, &set.Compiled)
	return set, err
}
