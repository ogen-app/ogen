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
	// AssistantActionIdeaEdit is a change to one idea's title, note or plan.
	AssistantActionIdeaEdit AssistantActionKind = "idea_edit"
	// AssistantActionIdeaVerdicts is several ideas triaged the same way at
	// once.
	AssistantActionIdeaVerdicts AssistantActionKind = "idea_verdicts"
	// AssistantActionPostCreated is a post the assistant made from ideas;
	// undoing it deletes the post while no one has changed it.
	AssistantActionPostCreated AssistantActionKind = "post_created"
	// AssistantActionBrandFact is a fact added to, changed in or removed
	// from the ledger.
	AssistantActionBrandFact AssistantActionKind = "brand_fact"
	// AssistantActionCampaignCreated is a campaign the assistant created;
	// undo deletes it while nothing was made in it.
	AssistantActionCampaignCreated AssistantActionKind = "campaign_created"
	// AssistantActionBrandVoice is a voice added, changed or removed.
	AssistantActionBrandVoice AssistantActionKind = "brand_voice"
	// AssistantActionBrandAudience is an audience added, changed or removed.
	AssistantActionBrandAudience AssistantActionKind = "brand_audience"
	// AssistantActionBrandGuardrails is a change to the guardrails' rules.
	AssistantActionBrandGuardrails AssistantActionKind = "brand_guardrails"
	// AssistantActionSeries is a series added, changed or removed.
	AssistantActionSeries AssistantActionKind = "series"
	// AssistantActionCampaignSeries is a campaign starting or stopping a
	// series, or running it at another rhythm.
	AssistantActionCampaignSeries AssistantActionKind = "campaign_series"
	// AssistantActionPostSchedules is posts of a campaign taken off the
	// calendar.
	AssistantActionPostSchedules AssistantActionKind = "post_schedules"
	// AssistantActionPostTypes is posts of a campaign changed to another
	// post type.
	AssistantActionPostTypes AssistantActionKind = "post_types"
	// AssistantActionAutoPublish is the workspace's auto-publish allowlist
	// changed.
	AssistantActionAutoPublish AssistantActionKind = "auto_publish"
	// AssistantActionCampaignGoal is the campaign's post goal changed.
	AssistantActionCampaignGoal AssistantActionKind = "campaign_goal"
	// AssistantActionAssetTags is tags added to or removed from assets.
	AssistantActionAssetTags AssistantActionKind = "asset_tags"
	// AssistantActionPostRewritten is a post's copy rewritten by a fix.
	AssistantActionPostRewritten AssistantActionKind = "post_rewritten"
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
