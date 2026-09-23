package models

import (
	"time"

	"github.com/uptrace/bun"
)

// FlowModelConfig is one (tier, flow, slot) -> model assignment (CON-308). It
// decides which foundation model a genkit flow slot uses, replacing the coarse
// 3-role provider built from env config.
//
// TierID nil = the GLOBAL-DEFAULT row (the required fallback used when a tenant's
// tier has no override for this slot). A non-nil TierID overrides the default for
// that one tier. Resolution is `tier-override ?? global-default`.
//
// GLOBAL operator table — NOT tenant-scoped — like TenantTier / PlatformGlobalLimits:
// read and written cross-tenant by operators over the internal gRPC surface, so
// it carries no tenant_id. The flow/slot catalog and the model catalog + prices
// live in code (the CON-86 vendor registry); only the assignment is persisted here.
type FlowModelConfig struct {
	bun.BaseModel `bun:"table:flow_model_config,alias:fmc" swaggerignore:"true"`

	ID        string    `bun:"id,pk"                                        json:"id"`
	TierID    *string   `bun:"tier_id"                                      json:"tier_id,omitempty"`
	FlowKey   string    `bun:"flow_key,notnull"                             json:"flow_key"`
	SlotKey   string    `bun:"slot_key,notnull"                             json:"slot_key"`
	ModelID   string    `bun:"model_id,notnull"                             json:"model_id"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	UpdatedBy string    `bun:"updated_by,notnull,default:''"                json:"updated_by"`
}

// IsGlobalDefault reports whether this is the global-default row (no tier scope).
func (c *FlowModelConfig) IsGlobalDefault() bool { return c.TierID == nil }
