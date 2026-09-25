package models

import (
	"context"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// CampaignType is a campaign's phase plan. System types (seeded, is_system) are
// shared by every workspace and have no owner; custom types belong to the
// workspace that created them (CON-314). It is deliberately not TenantScoped —
// that would hide the system types — so the repository scopes reads to "system
// or own" and writes to "own" by hand.
type CampaignType struct {
	bun.BaseModel `bun:"table:campaigns_types,alias:ct" swaggerignore:"true"`

	ID          string    `bun:"id,pk"               json:"id"`
	Name        string    `bun:"name,notnull"        json:"name"`
	Label       string    `bun:"label,notnull"       json:"label"`
	Description string    `bun:"description,notnull" json:"description"`
	IsSystem    bool      `bun:"is_system,notnull"   json:"is_system"`
	TenantID    *string   `bun:"tenant_id"           json:"-"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt   time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"-"`

	Phases []CampaignTypePhase `bun:"-" json:"phases,omitempty"`
}

var _ schema.BeforeAppendModelHook = (*CampaignType)(nil)

// BeforeAppendModel stamps the owner of a custom type on INSERT from the request
// tenant, like TenantScoped does — a write never carries its own tenant. A
// system context with no tenant trusts a caller-set owner and falls back to the
// default workspace. System types stay unowned.
func (m *CampaignType) BeforeAppendModel(ctx context.Context, query schema.Query) error {
	if _, ok := query.(*bun.InsertQuery); !ok || m.IsSystem {
		return nil
	}
	if tid, ok := tenantctx.From(ctx); ok {
		m.TenantID = &tid
		return nil
	}
	if tenantctx.IsSystem(ctx) {
		if m.TenantID == nil {
			tid := DefaultTenantID
			m.TenantID = &tid
		}
		return nil
	}
	return tenantctx.ErrNoTenant
}

type CampaignTypePhase struct {
	bun.BaseModel `bun:"table:campaigns_types_phases,alias:ctp" swaggerignore:"true"`

	ID             string    `bun:"id,pk"                                        json:"id"`
	CampaignTypeID string    `bun:"campaign_type_id,notnull"                     json:"campaign_type_id"`
	Name           string    `bun:"name,notnull"                                 json:"name"`
	Purpose        string    `bun:"purpose,notnull"                              json:"purpose"`
	Sequence       int       `bun:"sequence,notnull"                             json:"sequence"`
	CreatedAt      time.Time `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt      time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"-"`
}
