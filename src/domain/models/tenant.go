package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Tenant is the top-level isolation boundary. Every tenant-owned row
// carries a tenant_id referencing this table, and every user belongs to
// exactly one tenant. The slug is a human-friendly, globally-unique label; it
// is NOT used to resolve tenancy — isolation is derived from the session.
//
// Third-party dependency credentials are deliberately NOT modelled per tenant:
// one Ogen-wide, KEK-encrypted set of keys serves every tenant.
type Tenant struct {
	bun.BaseModel `bun:"table:tenants,alias:tn" swaggerignore:"true"`

	ID   string `bun:"id,pk"                                        json:"id"`
	Name string `bun:"name,notnull"                                 json:"name"`
	Slug string `bun:"slug,notnull,unique"                          json:"slug"`
	// TierID is the tenant's required classification tier. Backfilled
	// to DefaultTierID by the migration and stamped on every new signup, so it is
	// always set. FK is ON DELETE RESTRICT, so an assigned tier can't be deleted.
	// It — and the hydrated Tier/Groups below — is an OPERATOR classification kept
	// off the tenant-facing REST contract (json:"-"); it is surfaced only via the
	// Harbor gRPC TenantAdminService, which maps fields explicitly. Persistence is
	// unaffected (bun uses the `bun:` tag, not json).
	TierID string `bun:"tier_id,notnull"                              json:"-"`
	// Status is the tenant lifecycle state: active | suspended |
	// deleted. 'suspended' is a reversible operator freeze; 'deleted' coincides
	// with DeletedAt being set (invariant: status='deleted' <=> deleted_at IS NOT
	// NULL). Like TierID it is an OPERATOR field kept off the tenant-facing REST
	// contract (json:"-") and surfaced only via the Harbor gRPC TenantAdminService.
	// StatusReason records why a tenant is suspended; it is cleared on 'active'.
	Status       string    `bun:"status,notnull,default:'active'"              json:"-"`
	StatusReason string    `bun:"status_reason,notnull,default:''"             json:"-"`
	CreatedAt    time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt    time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	// DeletedAt marks a soft-deleted workspace. Membership
	// resolution filters it out, so a deleted workspace can't be listed, switched
	// to, or entered — but the row survives for support-side recovery. Plain
	// timestamp, NOT a bun `,soft_delete` column: see the migration comment.
	DeletedAt *time.Time `bun:"deleted_at" json:"deleted_at,omitempty"`

	// Tier and Groups are hydrated on the operator/admin read path only (CON-208
	// TenantAdminService), never scanned from the tenants table (bun:"-"), and —
	// like TierID — excluded from tenant-facing REST (json:"-"). The gRPC service
	// maps them explicitly, so it is unaffected by the json tag.
	Tier   *TenantTier   `bun:"-" json:"-"`
	Groups []TenantGroup `bun:"-" json:"-"`
}

// DefaultTenantID is the id of the tenant created by the multi-tenancy
// foundation migration. All data that predates CON-97 is backfilled onto it.
const DefaultTenantID = "default"

// Tenant lifecycle statuses. The set is closed and CHECK-enforced in
// the tenants table; the auth chokepoint and background-job guards treat
// anything other than TenantStatusActive as "not live".
const (
	TenantStatusActive    = "active"    // normal operation
	TenantStatusSuspended = "suspended" // reversible operator freeze
	TenantStatusDeleted   = "deleted"   // soft-deleted; coincides with DeletedAt
)
