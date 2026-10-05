package models

import "time"

// PostAttachTarget is a post as an attachment picker lists it: enough to
// recognise it, never its body.
type PostAttachTarget struct {
	ID              string     `bun:"id"               json:"id"`
	Title           string     `bun:"title"            json:"title"`
	Status          PostStatus `bun:"status"           json:"status"`
	PlatformID      string     `bun:"platform_id"      json:"platform_id"`
	PlatformName    string     `bun:"platform_name"    json:"platform"`
	CampaignID      string     `bun:"campaign_id"      json:"campaign_id"`
	CampaignName    string     `bun:"campaign_name"    json:"campaign_name"`
	UpdatedAt       time.Time  `bun:"updated_at"       json:"updated_at"`
	AttachmentCount int        `bun:"attachment_count" json:"attachment_count"`
}
