package clone

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// copyIdeaLinks links clone to the ideas its source was made from, in the
// source's order: a copy, or a platform version, comes from the same ideas.
func copyIdeaLinks(ctx context.Context, tx bun.Tx, clone *models.Post) error {
	if clone.ClonedFromPostID == nil {
		return nil
	}
	var links []models.PostIdea
	if err := tx.NewSelect().Model(&links).
		Where("pi.post_id = ?", *clone.ClonedFromPostID).
		OrderExpr("pi.position, pi.idea_id").
		Scan(ctx); err != nil {
		return err
	}
	if len(links) == 0 {
		return nil
	}
	for i := range links {
		links[i].PostID, links[i].CreatedAt = clone.ID, clone.CreatedAt
	}
	_, err := tx.NewInsert().Model(&links).Exec(ctx)
	return err
}
