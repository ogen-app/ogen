package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PostIdeaRepository stores which ideas each post was made from.
type PostIdeaRepository interface {
	// Link records that postID was made from ideaIDs, in that order, on db
	// (a transaction or the pool). A link that exists is kept.
	Link(ctx context.Context, db bun.IDB, postID string, ideaIDs []string) error
	// IdeasOfPost returns the ideas a post was made from, in order. Ideas
	// deleted since are gone from it.
	IdeasOfPost(ctx context.Context, postID string) ([]models.PostSourceIdea, error)
	// PostsOfIdea returns the posts made from an idea, oldest first.
	PostsOfIdea(ctx context.Context, ideaID string) ([]models.IdeaPostRef, error)
	// IdeasOfCampaignPosts maps each of a campaign's posts made from ideas
	// to their ids, in order.
	IdeasOfCampaignPosts(ctx context.Context, campaignID string) (map[string][]string, error)
}

type postIdeaRepository struct {
	db *bun.DB
}

// NewPostIdeaRepository returns a PostIdeaRepository over db.
func NewPostIdeaRepository(db *bun.DB) PostIdeaRepository {
	return &postIdeaRepository{db: db}
}

func (r *postIdeaRepository) Link(ctx context.Context, db bun.IDB, postID string, ideaIDs []string) error {
	if len(ideaIDs) == 0 {
		return nil
	}
	links := make([]models.PostIdea, len(ideaIDs))
	for i, id := range ideaIDs {
		links[i] = models.PostIdea{PostID: postID, IdeaID: id, Position: i}
	}
	_, err := db.NewInsert().Model(&links).On("CONFLICT (post_id, idea_id) DO NOTHING").Exec(ctx)
	return err
}

func (r *postIdeaRepository) IdeasOfPost(ctx context.Context, postID string) ([]models.PostSourceIdea, error) {
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	out := []models.PostSourceIdea{}
	err := r.db.NewSelect().Model((*models.PostIdea)(nil)).
		ColumnExpr("i.id, i.title, i.origin").
		Join("JOIN ideas AS i ON i.id = pi.idea_id AND i.tenant_id = ?", tenantID).
		Where("pi.post_id = ?", postID).
		OrderExpr("pi.position, i.id").
		Scan(ctx, &out)
	return out, err
}

func (r *postIdeaRepository) PostsOfIdea(ctx context.Context, ideaID string) ([]models.IdeaPostRef, error) {
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	out := []models.IdeaPostRef{}
	err := r.db.NewSelect().Model((*models.PostIdea)(nil)).
		ColumnExpr("po.id, po.title, po.status, po.campaign_id, coalesce(po.platform_id, '') AS platform_id, coalesce(po.platform_post_type, '') AS platform_post_type, po.scheduled_at").
		Join("JOIN posts AS po ON po.id = pi.post_id AND po.tenant_id = ?", tenantID).
		Where("pi.idea_id = ?", ideaID).
		OrderExpr("po.created_at, po.id").
		Scan(ctx, &out)
	return out, err
}

func (r *postIdeaRepository) IdeasOfCampaignPosts(ctx context.Context, campaignID string) (map[string][]string, error) {
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	var links []models.PostIdea
	err := r.db.NewSelect().Model(&links).
		Column("pi.post_id", "pi.idea_id").
		Join("JOIN posts AS po ON po.id = pi.post_id AND po.tenant_id = ?", tenantID).
		Where("po.campaign_id = ?", campaignID).
		OrderExpr("pi.post_id, pi.position, pi.idea_id").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string)
	for _, l := range links {
		out[l.PostID] = append(out[l.PostID], l.IdeaID)
	}
	return out, nil
}
