package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Caps on one campaign-tree listing.
const (
	MaxTreeCampaigns        = 100
	MaxTreePostsPerCampaign = 300
)

// campaignStatusRank orders a picker's campaigns: running work first, then
// what's about to run, then the rest.
const campaignStatusRank = `CASE c.status
	WHEN 'active' THEN 0
	WHEN 'scheduled' THEN 1
	WHEN 'draft' THEN 2
	WHEN 'paused' THEN 3
	WHEN 'completed' THEN 4
	ELSE 5 END`

// ListCampaignPostTree returns the tenant's live campaigns (not archived, not
// deleted) with their posts of every status nested beneath, for a picker.
// Campaigns are ordered by status (active, scheduled, draft, paused,
// completed, then anything else), newest start first with undated ones last,
// then name; posts by scheduled time with unscheduled ones last, then
// creation. maxCampaigns and maxPostsPerCampaign cap the result (<= 0 or
// above the package maxima means the maximum). Two queries; both scoped by
// the models' tenant hooks. No post body is read.
func (r *postRepository) ListCampaignPostTree(ctx context.Context, maxCampaigns, maxPostsPerCampaign int) ([]models.CampaignPostTree, error) {
	if maxCampaigns <= 0 || maxCampaigns > MaxTreeCampaigns {
		maxCampaigns = MaxTreeCampaigns
	}
	if maxPostsPerCampaign <= 0 || maxPostsPerCampaign > MaxTreePostsPerCampaign {
		maxPostsPerCampaign = MaxTreePostsPerCampaign
	}

	campaigns := []models.CampaignPostTree{}
	err := r.db.NewSelect().Model((*models.Campaign)(nil)).
		ColumnExpr(campaignTreeColumns).
		Where("c.deleted_at IS NULL").
		Where("c.archived_at IS NULL").
		Where("c.status <> ?", models.StatusArchived).
		OrderExpr(campaignStatusRank).
		OrderExpr("c.start_date DESC NULLS LAST, c.name, c.id").
		Limit(maxCampaigns).
		Scan(ctx, &campaigns)
	if err != nil {
		return nil, err
	}
	if len(campaigns) == 0 {
		return campaigns, nil
	}

	ids := make([]string, len(campaigns))
	byID := make(map[string]int, len(campaigns))
	for i := range campaigns {
		ids[i] = campaigns[i].ID
		byID[campaigns[i].ID] = i
		campaigns[i].Posts = []models.CampaignTreePost{}
	}

	posts, err := r.campaignTreePosts(ctx, ids, maxPostsPerCampaign)
	if err != nil {
		return nil, err
	}
	for _, p := range posts {
		if i, ok := byID[p.CampaignID]; ok {
			campaigns[i].Posts = append(campaigns[i].Posts, p)
		}
	}
	return campaigns, nil
}

// GetCampaignPostTree returns one of the tenant's campaigns with every one of
// its posts, ordered as in ListCampaignPostTree. Archived campaigns are
// returned; a deleted or unknown one is sql.ErrNoRows. A post that isn't
// listed is no longer in the campaign. No post body is read.
func (r *postRepository) GetCampaignPostTree(ctx context.Context, id string) (*models.CampaignPostTree, error) {
	campaign := new(models.CampaignPostTree)
	err := r.db.NewSelect().Model((*models.Campaign)(nil)).
		ColumnExpr(campaignTreeColumns).
		Where("c.id = ?", id).
		Where("c.deleted_at IS NULL").
		Scan(ctx, campaign)
	if err != nil {
		return nil, err
	}
	if campaign.Posts, err = r.campaignTreePosts(ctx, []string{id}, 0); err != nil {
		return nil, err
	}
	return campaign, nil
}

// campaignTreeColumns are the campaign fields of a CampaignPostTree.
const campaignTreeColumns = "c.id, c.name, c.status, c.timezone, c.start_date, c.end_date, c.posts_changed_at"

// campaignTreePosts returns the posts of campaignIDs, by scheduled time with
// unscheduled ones last, then creation. maxPerCampaign > 0 keeps the earliest
// that many per campaign; 0 keeps them all. The campaigns are already the
// tenant's, and the outer select is tenant-scoped as well.
func (r *postRepository) campaignTreePosts(ctx context.Context, campaignIDs []string, maxPerCampaign int) ([]models.CampaignTreePost, error) {
	q := r.db.NewSelect().Model((*models.Post)(nil)).
		ColumnExpr("po.id, po.campaign_id, po.title, po.status, po.platform_id, po.scheduled_at").
		ColumnExpr("COALESCE(po.platform_post_type, '') AS platform_post_type").
		ColumnExpr("COALESCE(pl.name, '') AS platform_name").
		ColumnExpr("(SELECT count(*) FROM post_attachments AS pa WHERE pa.post_id = po.id) AS attachment_count").
		ColumnExpr("(SELECT count(*) FROM post_attachments AS pa WHERE pa.post_id = po.id AND pa.mime_type LIKE 'video/%') AS video_count").
		Join("LEFT JOIN platforms AS pl ON pl.id = po.platform_id").
		OrderExpr("po.scheduled_at ASC NULLS LAST, po.created_at, po.id")
	if maxPerCampaign > 0 {
		q = q.Join(`JOIN (SELECT p2.id, row_number() OVER (
				PARTITION BY p2.campaign_id
				ORDER BY p2.scheduled_at ASC NULLS LAST, p2.created_at, p2.id
			) AS rn
			FROM posts AS p2 WHERE p2.campaign_id IN (?)) AS ranked
			ON ranked.id = po.id AND ranked.rn <= ?`, bun.List(campaignIDs), maxPerCampaign)
	} else {
		q = q.Where("po.campaign_id IN (?)", bun.List(campaignIDs))
	}
	posts := []models.CampaignTreePost{}
	if err := q.Scan(ctx, &posts); err != nil {
		return nil, err
	}
	return posts, nil
}
