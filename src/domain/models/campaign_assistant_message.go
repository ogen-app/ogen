package models

import (
	"time"

	"github.com/uptrace/bun"
)

// CampaignAssistantMessage is one turn in the per-campaign conversation between
// a user and the Campaign Assistant. Mirrors PostAssistantMessage but
// scoped to a campaign. TenantScoped auto-scopes every read/write to the
// request's tenant.
type CampaignAssistantMessage struct {
	bun.BaseModel `bun:"table:campaign_assistant_messages,alias:cam" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID         string    `bun:"id,pk"                                        json:"id"`
	CampaignID string    `bun:"campaign_id,notnull"                          json:"campaign_id"`
	Role       string    `bun:"role,notnull"                                 json:"role"`
	Content    string    `bun:"content,notnull"                              json:"content"`
	CreatedAt  time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}
