package models

import (
	"time"

	"github.com/uptrace/bun"
)

// TenantTier is an operator-owned classification a tenant is assigned to.
// Every tenant has exactly one, required tier. Tiers are a small,
// shared catalog (e.g. Free / Pro / Enterprise) managed by Harbor over the
// internal gRPC surface (src/grpc/server).
//
// Like Tenant, this is a GLOBAL table — NOT TenantScoped: it is read and
// written cross-tenant by operators, so it carries no tenant_id.
type TenantTier struct {
	bun.BaseModel `bun:"table:tenant_tiers,alias:tt" swaggerignore:"true"`

	ID          string    `bun:"id,pk"                                        json:"id"`
	Name        string    `bun:"name,notnull,unique"                          json:"name"`
	Color       string    `bun:"color,notnull,default:''"                     json:"color"`
	Description string    `bun:"description,notnull,default:''"                json:"description"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt   time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// DefaultTierID is the tier seeded by the CON-208 migration. Every pre-existing
// tenant is backfilled onto it and every new signup is stamped with it, so a
// tenant always has a tier. It is delete-protected in the service layer.
const DefaultTierID = "default"
