package models

import "time"

// CampaignPostTree is a campaign as a "send to" picker shows it: enough to
// recognise and order it, with its posts nested beneath. Never a post's body.
type CampaignPostTree struct {
	ID        string             `bun:"id"`
	Name      string             `bun:"name"`
	Status    CampaignStatus     `bun:"status"`
	Timezone  string             `bun:"timezone"`
	StartDate *time.Time         `bun:"start_date"`
	EndDate   *time.Time         `bun:"end_date"`
	Posts     []CampaignTreePost `bun:"-"`
}

// CampaignTreePost is one post under a CampaignPostTree. PlatformID,
// PlatformName and PlatformPostType are empty until the post has them.
// VideoCount is how many of its attachments are videos.
type CampaignTreePost struct {
	ID               string     `bun:"id"`
	CampaignID       string     `bun:"campaign_id"`
	Title            string     `bun:"title"`
	Status           PostStatus `bun:"status"`
	PlatformID       string     `bun:"platform_id"`
	PlatformName     string     `bun:"platform_name"`
	PlatformPostType string     `bun:"platform_post_type"`
	ScheduledAt      *time.Time `bun:"scheduled_at"`
	AttachmentCount  int        `bun:"attachment_count"`
	VideoCount       int        `bun:"video_count"`
}
