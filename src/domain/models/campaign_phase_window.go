package models

import (
	"time"

	"github.com/uptrace/bun"
)

// CampaignPhaseWindow is one phase's calendar-day range in a campaign's manual
// phase plan (CON-166). Rows exist only for a user-edited plan and are stored
// whole — one per phase of the campaign's type — or not at all; no rows means
// the plan is derived from the campaign dates (see domain/campaignphase).
type CampaignPhaseWindow struct {
	bun.BaseModel `bun:"table:campaign_phase_windows,alias:cpw" swaggerignore:"true"`
	TenantScoped

	CampaignID string    `bun:"campaign_id,pk"                               json:"campaign_id"`
	PhaseID    string    `bun:"phase_id,pk"                                  json:"phase_id"`
	StartDate  time.Time `bun:"start_date,notnull,type:date"                 json:"start_date"`
	EndDate    time.Time `bun:"end_date,notnull,type:date"                   json:"end_date"`
	CreatedAt  time.Time `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt  time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"-"`
}
