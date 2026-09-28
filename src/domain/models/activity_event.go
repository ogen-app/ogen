package models

import (
	"time"

	"github.com/uptrace/bun"
)

// ActivityEvent is one durable record of a meaningful user/tenant action:
// a post created, a signup, an AI flow run, a publish outcome, and
// so on. It lives in the isolated analytics database next to UsageEvent, and
// is the *behavioural* counterpart to metering: UsageEvent records vendor
// cost/tokens, ActivityEvent records that something happened, to what, by whom,
// and how it turned out. Some actions emit both — that is intentional.
//
// Like UsageEvent it carries a plain tenant_id with NO foreign key (the tenants
// table is in the other database); isolation is enforced app-side by the
// embedded TenantScoped hooks. The async activity.Recorder writes each row in a
// system context with TenantID pre-set (preserved by BeforeAppendModel), and
// read queries run in the caller's tenant context so they auto-scope.
type ActivityEvent struct {
	bun.BaseModel `bun:"table:tenant_activity_events,alias:ae" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks (no FK in the analytics DB)

	ID     string `bun:"id,notnull"       json:"id"`
	UserID string `bun:"user_id,nullzero" json:"user_id,omitempty"` // empty for system/background activities

	// Category is the primary tag (the main slice dimension): post |
	// authentication | ai_flow | campaign | publish | … Type is the specific
	// verb within a category: post_created, signup, content_plan, …
	Category string `bun:"category,notnull" json:"category"`
	Type     string `bun:"type,notnull"     json:"type"`

	// Entity the action targeted (post | campaign | user | tenant | …) and its
	// id. Both empty for actions without a single subject.
	EntityType string `bun:"entity_type,nullzero" json:"entity_type,omitempty"`
	EntityID   string `bun:"entity_id,nullzero"   json:"entity_id,omitempty"`

	// Status is the outcome or resulting state (success | failed |
	// draft->scheduled | …); Source is how it was triggered (api | assistant |
	// job | system).
	Status string `bun:"status,nullzero" json:"status,omitempty"`
	Source string `bun:"source,nullzero" json:"source,omitempty"`

	// Tags are optional secondary labels; Payload is the activity-specific bag
	// of fields (status_from/to, platform, model, counts, latency_ms, error, …).
	Tags    StringSlice `bun:"tags,nullzero,type:jsonb"    json:"tags,omitempty"`
	Payload JSONMap     `bun:"payload,nullzero,type:jsonb" json:"payload,omitempty"`

	// OccurredAt is the event time, snapshotted by the Recorder at record time —
	// NOT insert time, since the write is async/batched. Intentionally not
	// nullzero (mirrors UsageEvent.OccurredAt): the recorder always sets it, and
	// a zero value should surface as 0001-01-01 rather than silently taking the
	// later flush time via the column default.
	OccurredAt time.Time `bun:"occurred_at,notnull,default:current_timestamp" json:"occurred_at"`
}
