package models

import (
	"time"

	"github.com/uptrace/bun"
)

// CampaignStatus represents the lifecycle state of a campaign.
type CampaignStatus string

const (
	StatusDraft     CampaignStatus = "draft"
	StatusScheduled CampaignStatus = "scheduled"
	StatusActive    CampaignStatus = "active"
	StatusPaused    CampaignStatus = "paused"
	StatusCompleted CampaignStatus = "completed"
	StatusArchived  CampaignStatus = "archived"
)

type Campaign struct {
	bun.BaseModel `bun:"table:campaigns,alias:c" swaggerignore:"true"`
	TenantScoped  // CON-97: tenant_id column + central scoping hooks

	ID   string `bun:"id,pk"                                        json:"id"`
	Name string `bun:"name,notnull"                                 json:"name"`

	// meta
	Description    string `bun:"description,notnull"                          json:"description"`
	TargetPersona  string `bun:"target_persona,notnull"                       json:"target_persona"`
	KeyMessages    string `bun:"key_messages,notnull"                         json:"key_messages"`
	ToneGuidelines string `bun:"tone_guidelines,notnull"                      json:"tone_guidelines"`
	// Brand bindings (CON-245): the voice/audience this campaign writes in.
	// Nullable — resolution falls back to the workspace default voice /
	// tone_guidelines prose when unset. FK ON DELETE SET NULL.
	BrandVoiceID       *string           `bun:"brand_voice_id"                               json:"brand_voice_id"`
	BrandAudienceID    *string           `bun:"brand_audience_id"                            json:"brand_audience_id"`
	UseAssets          bool              `bun:"use_assets,notnull"                           json:"use_assets"`
	AssetIDs           StringSlice       `bun:"asset_ids,notnull,type:jsonb"                 json:"asset_ids"`
	TargetPlatforms    CampaignPlatforms `bun:"target_platforms,notnull,type:jsonb"          json:"target_platforms"`
	CampaignTypeID     string            `bun:"campaign_type_id,notnull"                     json:"campaign_type_id"`
	StartDate          *time.Time        `bun:"start_date"                                   json:"start_date"`
	EndDate            *time.Time        `bun:"end_date"                                     json:"end_date"`
	EstimatedPostCount *int              `bun:"estimated_post_count"                         json:"estimated_post_count"`
	Language           string            `bun:"language,notnull"                             json:"language"`

	// Scheduling settings (CON-181) — consumed by the content-plan flow to
	// place each generated draft's scheduled_at. PublishingTime is a local
	// "HH:MM" wall clock; Timezone is an IANA name ("" = UTC); PublishingDays
	// is a subset of mon..sun; SpreadMinutes is the ± jitter (0 = exact).
	PublishingTime string      `bun:"publishing_time,notnull,default:'09:00'"      json:"publishing_time"`
	Timezone       string      `bun:"timezone,notnull,default:''"                  json:"timezone"`
	PublishingDays StringSlice `bun:"publishing_days,notnull,type:jsonb"           json:"publishing_days"`
	SpreadMinutes  int         `bun:"spread_minutes,notnull,default:15"            json:"spread_minutes"`

	// Goal (CON-182). estimated_post_count above is the target number of posts
	// PER goal_cadence period; the content-plan flow multiplies it by the number
	// of week/month periods the campaign's [start_date, end_date] window spans,
	// and the campaign overview reports per-period progress against it.
	GoalCadence string `bun:"goal_cadence,notnull,default:'month'"          json:"goal_cadence"`

	// Lifecycle (CON-156 BE 6). A campaign leaves the active set by being
	// archived (reversible) or soft-deleted (an operational safety net, not an
	// undo — no self-serve restore). Both are nullable timestamps filtered
	// explicitly in the repository rather than via bun's soft_delete tag, so
	// archive and delete stay independent: GetByID still returns an archived
	// campaign (to unarchive it) while hiding a deleted one.
	ArchivedAt *time.Time `bun:"archived_at"                                  json:"archived_at"`
	DeletedAt  *time.Time `bun:"deleted_at"                                   json:"deleted_at"`

	// system
	Status       CampaignStatus `bun:"status,notnull"                               json:"status"`
	Budget       *float64       `bun:"budget"                                       json:"budget"`
	Currency     string         `bun:"currency,notnull"                             json:"currency"`
	TagIDs       StringSlice    `bun:"tag_ids,notnull,type:jsonb"                   json:"tag_ids"`
	Tags         []Tag          `bun:"-"                                            json:"tags"`
	Platforms    []Platform     `bun:"-"                                            json:"platforms"`
	CampaignType *CampaignType  `bun:"-"                                            json:"campaign_type,omitempty"`
	CreatedBy    string         `bun:"created_by,notnull"                           json:"created_by"`
	CreatedAt    time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt    time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Phase plan (CON-166). TypeLocked: campaign_type_id can no longer change
	// because posts are planned against the type's phases (PhasedPostCount > 0);
	// hydrated on reads. PhaseWindows is the stored manual phase plan, hydrated
	// on GetByID — empty means derived; read it via campaignphase.Resolve.
	// PhasePlanReset is set on a PUT response when a type/date change dropped
	// the manual plan back to derived.
	TypeLocked      bool                  `bun:"-" json:"type_locked"`
	PhasedPostCount int                   `bun:"-" json:"-"`
	PhaseWindows    []CampaignPhaseWindow `bun:"-" json:"-"`
	PhasePlanReset  bool                  `bun:"-" json:"phase_plan_reset,omitempty"`
}
