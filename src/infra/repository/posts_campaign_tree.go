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
		ColumnExpr("c.id, c.name, c.status, c.timezone, c.start_date, c.end_date").
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

	// The ranking subquery only sees posts of the campaigns above, which
	// are already the tenant's; the outer select is tenant-scoped as well.
	posts := []models.CampaignTreePost{}
	err = r.db.NewSelect().Model((*models.Post)(nil)).
		ColumnExpr("po.id, po.campaign_id, po.title, po.status, po.platform_id, po.scheduled_at").
		ColumnExpr("COALESCE(po.platform_post_type, '') AS platform_post_type").
		ColumnExpr("COALESCE(pl.name, '') AS platform_name").
		ColumnExpr("(SELECT count(*) FROM post_attachments AS pa WHERE pa.post_id = po.id) AS attachment_count").
		ColumnExpr("(SELECT count(*) FROM post_attachments AS pa WHERE pa.post_id = po.id AND pa.mime_type LIKE 'video/%') AS video_count").
		Join(`JOIN (SELECT p2.id, row_number() OVER (
				PARTITION BY p2.campaign_id
				ORDER BY p2.scheduled_at ASC NULLS LAST, p2.created_at, p2.id
			) AS rn
			FROM posts AS p2 WHERE p2.campaign_id IN (?)) AS ranked
			ON ranked.id = po.id AND ranked.rn <= ?`, bun.List(ids), maxPostsPerCampaign).
		Join("LEFT JOIN platforms AS pl ON pl.id = po.platform_id").
		OrderExpr("po.scheduled_at ASC NULLS LAST, po.created_at, po.id").
		Scan(ctx, &posts)
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
