package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AssistantActionKind names what a reversible assistant change touched, and
// so which undoer restores it.
type AssistantActionKind string

const (
	// AssistantActionCampaignDates is a change to a campaign's start/end dates.
	AssistantActionCampaignDates AssistantActionKind = "campaign_dates"
	// AssistantActionCampaignBrief is a change to a campaign's brief fields.
	AssistantActionCampaignBrief AssistantActionKind = "campaign_brief"
	// AssistantActionCampaignPlatforms is a change to the platforms a
	// campaign targets and the post types it uses on them.
	AssistantActionCampaignPlatforms AssistantActionKind = "campaign_platforms"
	// AssistantActionCampaignAssets is a change to the assets a campaign
	// draws on.
	AssistantActionCampaignAssets AssistantActionKind = "campaign_assets"
	// AssistantActionIdeaVerdict is a change to one idea's triage verdict.
	AssistantActionIdeaVerdict AssistantActionKind = "idea_verdict"
	// AssistantActionBrandFact is a fact added to, changed in or removed
	// from the ledger.
	AssistantActionBrandFact AssistantActionKind = "brand_fact"
)

// AssistantAction is a reversible change the assistant made: the entity's
// state before and after. Undo restores Before only while the entity still
// matches After, so a later edit is never overwritten.
type AssistantAction struct {
	bun.BaseModel `bun:"table:assistant_actions,alias:asa" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string              `bun:"id,pk"                       json:"id"`
	ScopeKind AssistantScopeKind  `bun:"scope_kind,notnull"          json:"scope_kind"`
	ScopeID   string              `bun:"scope_id,notnull"            json:"scope_id"`
	Kind      AssistantActionKind `bun:"kind,notnull"                json:"kind"`
	EntityID  string              `bun:"entity_id,notnull"           json:"entity_id"`
	Before    JSONMap             `bun:"before_state,type:jsonb,notnull" json:"before"`
	After     JSONMap             `bun:"after_state,type:jsonb,notnull"  json:"after"`
	CreatedBy string              `bun:"created_by,notnull"          json:"created_by"`
	// CreatedAt is set by the database.
	CreatedAt time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UndoneAt  *time.Time `bun:"undone_at"                                     json:"undone_at,omitempty"`
	UndoneBy  string     `bun:"undone_by,notnull"                             json:"undone_by,omitempty"`
}
