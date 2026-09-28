package models

import (
	"time"

	"github.com/uptrace/bun"
)

// UsageEvent is one durable record of a metered vendor call — a model
// generation/embedding or a publisher action. It lives in
// the isolated analytics database, not the control-plane Postgres, so it
// carries a plain tenant_id with NO foreign key to tenants (that table is in
// the other database). Cross-tenant isolation is still enforced app-side by
// the embedded TenantScoped hooks: the recorder writes each row in a system
// context with TenantID pre-set (preserved by BeforeAppendModel), and read
// queries run in the caller's tenant context so they auto-scope.
//
// The four common token kinds are broken out as columns so the TimescaleDB
// daily rollup can sum them cheaply; rarer kinds (reasoning, and the
// publisher kinds post/api_call/account) live in the ExtraUnits jsonb so a
// new vendor never needs a schema change.
type UsageEvent struct {
	bun.BaseModel `bun:"table:vendor_usage_events,alias:ue" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks (no FK in the analytics DB)

	ID        string `bun:"id,notnull"        json:"id"`
	Vendor    string `bun:"vendor,notnull"    json:"vendor"`    // anthropic | gemini | zernio | …
	Family    string `bun:"family,notnull"    json:"family"`    // model | publisher
	Model     string `bun:"model,notnull"     json:"model"`     // model id or publisher sku/tier
	Operation string `bun:"operation,notnull" json:"operation"` // generate | embed | publish | account_connect | …
	Feature   string `bun:"feature,notnull"   json:"feature"`   // content_plan | asset_embed | auto_publish | …

	// Publisher dimensions; empty for model events.
	Platform        string `bun:"platform,nullzero"          json:"platform,omitempty"`
	SocialAccountID string `bun:"social_account_id,nullzero" json:"social_account_id,omitempty"`

	// Common token kinds, broken out for fast rollups.
	InputTokens         int64 `bun:"input_tokens,notnull"          json:"input_tokens"`
	OutputTokens        int64 `bun:"output_tokens,notnull"         json:"output_tokens"`
	CacheReadTokens     int64 `bun:"cache_read_tokens,notnull"     json:"cache_read_tokens"`
	CacheCreationTokens int64 `bun:"cache_creation_tokens,notnull" json:"cache_creation_tokens"`

	// Long-tail kinds (reasoning, post, api_call, account, …) keyed by kind.
	ExtraUnits map[string]int64 `bun:"extra_units,nullzero,type:jsonb" json:"extra_units,omitempty"`

	// CostMicros is the snapshot cost computed from the price map at write
	// time (USD-millionths). PriceVersion records which rate set produced it,
	// so historical cost is never recomputed.
	CostMicros   int64  `bun:"cost_micros,notnull"    json:"cost_micros"`
	PriceVersion string `bun:"price_version,nullzero" json:"price_version,omitempty"`

	// OccurredAt is the event time, snapshotted by the Recorder at record time —
	// NOT insert time, since the write is async/batched. Intentionally not
	// nullzero: the recorder always sets it, and were it ever left zero we want
	// the glaring 0001-01-01 over the default current_timestamp, which would
	// silently stamp the later flush time and mis-bucket usage + enforcement
	// windows. So do not "harden" this with nullzero (unlike audit created_at).
	OccurredAt time.Time `bun:"occurred_at,notnull,default:current_timestamp" json:"occurred_at"`
}
