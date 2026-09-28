package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Limit modes: enforce blocks a synchronous call once the
// tenant is already over a cap; warn records + counts but proceeds.
const (
	LimitModeEnforce = "enforce"
	LimitModeWarn    = "warn"
)

// TenantUsageLimit is a tenant's per-day / per-month spend cap and mode,
// stored in the control-plane database. One row per tenant
// (UNIQUE on tenant_id); an absent row means "fall back to the config
// defaults, else unlimited". Nullable caps mean "no cap for that period".
type TenantUsageLimit struct {
	bun.BaseModel `bun:"table:tenant_usage_limits,alias:tul" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID string `bun:"id,pk" json:"id"`

	DailyCapMicros   *int64 `bun:"daily_cap_micros,nullzero"   json:"daily_cap_micros"`
	MonthlyCapMicros *int64 `bun:"monthly_cap_micros,nullzero" json:"monthly_cap_micros"`
	Mode             string `bun:"mode,notnull"                json:"mode"` // enforce | warn
	Enabled          bool   `bun:"enabled,notnull"             json:"enabled"`

	// nullzero so the Upsert (which never sets CreatedAt — only UpdatedAt) lets
	// the DB default apply on insert instead of bun writing a zero time. With a
	// default tag, bun emits SQL DEFAULT for the zero value, so the NOT NULL
	// column still gets now(). UpdatedAt is always stamped by the repo, so it
	// needs no such treatment.
	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
