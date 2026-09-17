// Package summaries computes the batched Campaigns-list payload (CON-152): a
// slim per-post projection for every campaign in the tenant, grouped by
// campaign, in one response. It exists to kill the N+1 the revamped list
// created — one CampaignCard per campaign, each firing its own
// GET /api/campaigns/:id/posts just to derive a badge, a few stat tiles, and a
// stage line (CON-127).
//
// The projection carries only the fields the client's lib/campaignReadiness
// rules read, so the list keeps running the exact same rules as the Campaign
// Overview screen (no reduced rule set, no server-side rule port) while the
// heavy title/content/relations never leave the database.
package summaries

import "time"

// PostSummary is the slim per-post projection. Field names mirror the REST Post
// shape (snake_case) so the client feeds these straight into the readiness
// rules that a full Post would satisfy. No title, content, or hydrated
// relations — see the package doc.
type PostSummary struct {
	ID                  string     `json:"id"`
	CampaignID          string     `json:"campaign_id"`
	Status              string     `json:"status"`
	ScheduledAt         *time.Time `json:"scheduled_at"`
	PublishedAt         *time.Time `json:"published_at"`
	PlatformID          string     `json:"platform_id"`
	PlatformPostType    string     `json:"platform_post_type"`
	CampaignTypePhaseID *string    `json:"campaign_type_phase_id"`
	MediaURLs           []string   `json:"media_urls"`
	CreatedBy           string     `json:"created_by"`
	FailureReason       string     `json:"failure_reason"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// CampaignSummary is one campaign's projected posts. A campaign with no posts
// is simply absent from the response; the client defaults a missing campaign to
// an empty list and keys load-state off the whole-request success.
type CampaignSummary struct {
	CampaignID string        `json:"campaign_id"`
	Posts      []PostSummary `json:"posts"`
}

// Summaries is the batched response: every campaign that has posts, ordered by
// campaign id, plus a generation stamp (mirrors overview.Overview).
type Summaries struct {
	Summaries   []CampaignSummary `json:"summaries"`
	GeneratedAt time.Time         `json:"generated_at"`
}
