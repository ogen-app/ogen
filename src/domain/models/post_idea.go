package models

import (
	"time"

	"github.com/uptrace/bun"
)

// PostIdea links a post to one of the ideas it was made from. A post can come
// from several ideas and an idea can feed several posts.
type PostIdea struct {
	bun.BaseModel `bun:"table:post_ideas,alias:pi" swaggerignore:"true"`
	TenantScoped

	PostID string `bun:"post_id,pk"`
	IdeaID string `bun:"idea_id,pk"`
	// Position orders a post's ideas as they were compiled.
	Position  int       `bun:"position,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

// PostSourceIdea is an idea a post was made from, as the post shows it.
type PostSourceIdea struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Origin IdeaOrigin `json:"origin" swaggertype:"string" enums:",asset_analysis,content_plan,assistant"`
}

// IdeaSlot is a dated idea on the calendar: a day an idea plans to land on,
// drawn beside the posts but never one of them. Kind is always "idea", so
// the client can mix slots and posts in one day.
type IdeaSlot struct {
	Kind         string       `json:"kind" enums:"idea"`
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Date         CalendarDate `json:"date" swaggertype:"string" example:"2026-12-01"`
	CampaignID   *string      `json:"campaign_id" extensions:"x-nullable"`
	CampaignName string       `json:"campaign_name"`
	PlatformID   *string      `json:"platform_id" extensions:"x-nullable"`
	PostType     *string      `json:"platform_post_type" extensions:"x-nullable"`
	PhaseID      *string      `json:"phase_id" extensions:"x-nullable"`
	Origin       IdeaOrigin   `json:"origin" swaggertype:"string" enums:",asset_analysis,content_plan,assistant"`
	// Verdict is nil while the idea waits for triage.
	Verdict *IdeaVerdict `json:"verdict" swaggertype:"string" enums:"yes,later" extensions:"x-nullable"`
}

// IdeaSlotKind is IdeaSlot.Kind.
const IdeaSlotKind = "idea"

// IdeaPostRef is a post made from an idea, as the idea shows it.
type IdeaPostRef struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Status      PostStatus `json:"status"`
	CampaignID  string     `json:"campaign_id"`
	PlatformID  string     `json:"platform_id"`
	ScheduledAt *time.Time `json:"scheduled_at" extensions:"x-nullable"`
}
