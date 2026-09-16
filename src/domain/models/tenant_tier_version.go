package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Tier-version lifecycle statuses (CON-243). A version is authored as a draft,
// published to active, and eventually retired. Only draft rows are mutable — the
// tenant_tier_versions immutability trigger freezes the rest (see the migration).
const (
	TierVersionStatusDraft   = "draft"
	TierVersionStatusActive  = "active"
	TierVersionStatusRetired = "retired"
)

// Assignment reasons (CON-243) — why a tenant landed on a tier version. Mirrors
// the reason CHECK on tenant_tier_assignments.
const (
	AssignmentReasonSignup            = "signup"
	AssignmentReasonMigrationAccepted = "migration_accepted"
	AssignmentReasonMigrationLapsed   = "migration_lapsed"
	AssignmentReasonGrandfathered     = "grandfathered"
	AssignmentReasonUpgrade           = "upgrade"
	AssignmentReasonDowngrade         = "downgrade"
	// AssignmentReasonOperatorSet marks an assignment stamped by an operator
	// setting a tenant's tier directly via Harbor (CON-294 SetTenantTier).
	AssignmentReasonOperatorSet = "operator_set"
)

// TenantTierVersion is an immutable, versioned snapshot of one tier's pricing +
// entitlements (CON-243). It hangs off the CON-208 tenant_tiers row (the
// "plan"); a tenant is bound to a specific version over time via
// tenant_tier_assignments. GLOBAL operator table — not TenantScoped.
//
// Entitlements is a map keyed by the embedded feature catalog
// (src/domain/entitlements). A numeric value of nil means "unlimited"; a bool
// value gates a capability. JSON numbers decode as float64 (bun uses
// encoding/json for the jsonb column), so consumers coerce as needed.
type TenantTierVersion struct {
	bun.BaseModel `bun:"table:tenant_tier_versions,alias:ttv" swaggerignore:"true"`

	ID           string         `bun:"id,pk"                                        json:"id"`
	TierID       string         `bun:"tier_id,notnull"                              json:"tier_id"`
	Version      int            `bun:"version,notnull"                              json:"version"`
	Status       string         `bun:"status,notnull,default:'draft'"               json:"status"`
	Purchasable  bool           `bun:"purchasable,notnull,default:false"            json:"purchasable"`
	ChangeReason string         `bun:"change_reason,notnull,default:''"             json:"change_reason"`
	Entitlements map[string]any `bun:"entitlements,type:jsonb,notnull"              json:"entitlements"`
	CreatedAt    time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	PublishedAt  *time.Time     `bun:"published_at"                                 json:"published_at,omitempty"`
	RetiredAt    *time.Time     `bun:"retired_at"                                   json:"retired_at,omitempty"`
}

// TenantTierVersionPrice is one net (VAT-exclusive) price for a tier version, in
// ISO-4217 minor units (CON-243). A nil CountryCode row is the default for its
// currency/interval. GLOBAL operator table.
type TenantTierVersionPrice struct {
	bun.BaseModel `bun:"table:tenant_tier_version_prices,alias:ttvp" swaggerignore:"true"`

	ID              string  `bun:"id,pk"                    json:"id"`
	TierVersionID   string  `bun:"tier_version_id,notnull"  json:"tier_version_id"`
	Currency        string  `bun:"currency,notnull"         json:"currency"`
	BillingInterval string  `bun:"billing_interval,notnull" json:"billing_interval"`
	NetMinor        int64   `bun:"net_minor,notnull"        json:"net_minor"`
	CountryCode     *string `bun:"country_code"             json:"country_code,omitempty"`
}

// TenantTierAssignment binds a tenant to a specific tier version over a validity
// range (CON-243). Append-only; a gist exclusion constraint forbids overlapping
// ranges for one tenant, so point-in-time resolution is unambiguous. The
// open-ended row (ValidTo nil = upper infinity) is the tenant's current
// assignment. GLOBAL operator table.
//
// The tstzrange `valid` column has no native Go type, so the repository writes
// it with a raw tstzrange(...) expression and reads it back through the
// scan-only ValidFrom/ValidTo projections (lower()/upper()).
type TenantTierAssignment struct {
	bun.BaseModel `bun:"table:tenant_tier_assignments,alias:tta" swaggerignore:"true"`

	ID            string     `bun:"id,pk"                                        json:"id"`
	TenantID      string     `bun:"tenant_id,notnull"                            json:"tenant_id"`
	TierVersionID string     `bun:"tier_version_id,notnull"                      json:"tier_version_id"`
	Reason        string     `bun:"reason,notnull"                               json:"reason"`
	NoticeID      *string    `bun:"notice_id"                                    json:"notice_id,omitempty"`
	CreatedAt     time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	ValidFrom     *time.Time `bun:"valid_from,scanonly"                          json:"valid_from,omitempty"`
	ValidTo       *time.Time `bun:"valid_to,scanonly"                            json:"valid_to,omitempty"`
}

// VersionAssignment is a scan-only projection of one tenant's live (open-ended)
// assignment on a tier version, joined to the tenant's display name (CON-297).
// It backs the operator's "who is on this version" read used before retiring.
type VersionAssignment struct {
	TenantID   string    `bun:"tenant_id"   json:"tenant_id"`
	TenantName string    `bun:"tenant_name" json:"tenant_name"`
	ValidFrom  time.Time `bun:"valid_from"  json:"valid_from"`
}
