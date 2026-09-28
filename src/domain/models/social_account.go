package models

import (
	"time"

	"github.com/uptrace/bun"
)

// SocialAccount is the local view of a Zernio-connected account.
// The primary key matches Zernio's accountId so reconciler
// upserts can key off it directly.
//
// Soft-delete is the default disconnect path: DeletedAt set to a
// non-nil time means the account was removed on Zernio's side, and
// the row is preserved so future Pieces / Posts that reference it
// keep their FK integrity.
type SocialAccount struct {
	bun.BaseModel `bun:"table:social_accounts,alias:sa" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID           string     `bun:"id,pk"                 json:"id"`
	Platform     string     `bun:"platform,notnull"      json:"platform"`
	ProfileID    string     `bun:"profile_id,notnull"    json:"profile_id"`
	Username     string     `bun:"username,notnull"      json:"username"`
	DisplayName  string     `bun:"display_name,notnull"  json:"display_name"`
	AvatarURL    string     `bun:"avatar_url,notnull"    json:"avatar_url"`
	IsActive     bool       `bun:"is_active,notnull"     json:"is_active"`
	RawJSON      string     `bun:"raw_json,notnull"      json:"-"`
	ConnectedAt  time.Time  `bun:"connected_at,notnull"  json:"connected_at"`
	LastSyncedAt time.Time  `bun:"last_synced_at,notnull" json:"last_synced_at"`
	DeletedAt    *time.Time `bun:"deleted_at,nullzero"   json:"deleted_at,omitempty"`

	// Health snapshot. Populated by the detect_expiring_connections
	// sweep from Zernio's GET /v1/accounts/health; all nullable and NULL until
	// the first sweep. TokenExpiresAt is the forward-looking token expiry that
	// drives the "connection expiring" owner notification; HealthStatus is
	// Zernio's healthy/warning/error verdict. Kept out of the reconciler's
	// upsert set so a sync never overwrites them.
	TokenExpiresAt      *time.Time `bun:"token_expires_at,nullzero"       json:"token_expires_at,omitempty"`
	TokenValid          *bool      `bun:"token_valid,nullzero"            json:"token_valid,omitempty"`
	HealthStatus        string     `bun:"health_status,nullzero"          json:"health_status,omitempty"`
	NeedsReconnect      *bool      `bun:"needs_reconnect,nullzero"        json:"needs_reconnect,omitempty"`
	LastHealthCheckedAt *time.Time `bun:"last_health_checked_at,nullzero" json:"last_health_checked_at,omitempty"`
}
