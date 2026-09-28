package models

import (
	"time"

	"github.com/uptrace/bun"
)

// FollowerSnapshot is one daily follower-count datum for a connected social
// account, persisted append-only in the isolated analytics DB.
// Zernio refreshes follower counts once per day; the refresh_zernio_followers
// job records one row per (account, day) so Ogen keeps a durable follower
// history beyond Zernio's own window. There is one row per (tenant, account,
// day) — the daily upsert keys on (tenant_id, social_account_id, point_date).
//
// platform/username are denormalised (this DB is isolated from
// social_accounts). No FK on tenant_id/social_account_id; tenant isolation is
// enforced app-side by the embedded TenantScoped hooks, exactly like
// PostAnalytics.
type FollowerSnapshot struct {
	bun.BaseModel `bun:"table:follower_stats_snapshots,alias:fs" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks (no FK in the analytics DB)

	ID               string  `bun:"id,notnull"                json:"-"`
	SocialAccountID  string  `bun:"social_account_id,notnull" json:"account_id"`
	Platform         string  `bun:"platform,nullzero"         json:"platform"`
	Username         string  `bun:"username,nullzero"         json:"username"`
	Followers        int64   `bun:"followers,notnull"         json:"followers"`
	Growth           int64   `bun:"growth,notnull"            json:"-"`
	GrowthPercentage float64 `bun:"growth_percentage,notnull" json:"-"`
	// PointDate is the calendar day of the follower count (Zernio's data-point
	// date, UTC midnight). The (social_account_id, point_date) pair is the
	// idempotency key for the daily upsert and the query axis for the series.
	PointDate time.Time `bun:"point_date,notnull" json:"date"`
	RawJSON   string    `bun:"raw_json,nullzero"  json:"-"`
	// RecordedAt is the refresh time; the hypertable is partitioned on point_date.
	RecordedAt time.Time `bun:"recorded_at,notnull,default:current_timestamp" json:"-"`
}
