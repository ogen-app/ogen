package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// PostAnalyticsMetrics is the aggregate engagement block surfaced in
// analytics API responses. The same shape is embedded inside
// each per-platform breakdown row. On the stored PostAnalytics row these
// aggregates live in their own columns (for sortable list/overview
// queries); this struct is the assembled JSON representation.
type PostAnalyticsMetrics struct {
	Impressions    int     `json:"impressions"`
	Reach          int     `json:"reach"`
	Likes          int     `json:"likes"`
	Comments       int     `json:"comments"`
	Shares         int     `json:"shares"`
	Saves          int     `json:"saves"`
	Clicks         int     `json:"clicks"`
	Views          int     `json:"views"`
	EngagementRate float64 `json:"engagement_rate"`
}

// PostPlatformAnalytics is one per-platform row of the breakdown stored
// in post_analytics_current.platform_analytics. SyncStatus /
// ErrorMessage / ReauthorizeURL carry the per-platform scope-gap nuance
// (412/reauthorizeUrl): a platform connected before analytics scopes
// were granted surfaces its error here without failing the whole
// snapshot, while platforms that do report still carry metrics.
type PostPlatformAnalytics struct {
	Platform        string               `json:"platform"`
	Status          string               `json:"status,omitempty"`
	PlatformPostID  string               `json:"platform_post_id,omitempty"`
	AccountID       string               `json:"account_id,omitempty"`
	AccountUsername string               `json:"account_username,omitempty"`
	PlatformPostURL string               `json:"platform_post_url,omitempty"`
	SyncStatus      string               `json:"sync_status,omitempty"`
	ErrorMessage    string               `json:"error_message,omitempty"`
	ReauthorizeURL  string               `json:"reauthorize_url,omitempty"`
	Analytics       PostAnalyticsMetrics `json:"analytics"`
}

// PlatformAnalyticsList is a []PostPlatformAnalytics that serialises as a
// JSON array in a jsonb column (mirrors StringSlice / CampaignPlatforms).
type PlatformAnalyticsList []PostPlatformAnalytics

func (l PlatformAnalyticsList) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	b, err := json.Marshal(l)
	return string(b), err
}

func (l *PlatformAnalyticsList) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*l = PlatformAnalyticsList{}
			return nil
		}
		return json.Unmarshal([]byte(v), l)
	case []byte:
		if len(v) == 0 {
			*l = PlatformAnalyticsList{}
			return nil
		}
		return json.Unmarshal(v, l)
	case nil:
		*l = PlatformAnalyticsList{}
		return nil
	default:
		return fmt.Errorf("PlatformAnalyticsList: cannot scan %T", src)
	}
}

// PostAnalytics is the CURRENT engagement state for one Post published through
// a publisher (CON-93; storage reworked in CON-236). It lives in the isolated
// analytics database as ONE row per post (table post_analytics_current): the
// refresh queue UPSERTs it every time it checks a post, so it always reflects
// the newest numbers and every analytics read is a plain indexed scan — no
// latest-per-post subquery. The append-only trend history lives separately in
// post_analytics_snapshots (PostAnalyticsSnapshot), written only when the
// metrics actually change (CON-236 dedup).
//
// The post display fields (Publisher/Platform/Title/PublishedAt) are
// DENORMALISED here because the analytics DB can't JOIN across to
// posts/platforms; they are refreshed to the current post values on each check.
// The aggregate metrics are their own columns (sortable in the list/overview);
// PlatformAnalytics holds the LATEST per-platform breakdown as JSON.
// MetricsLastUpdated is the publisher's own "numbers last computed" timestamp.
// LastCheckedAt is when Ogen last fetched (bumped every check, so freshness
// survives dedup); LastChangedAt is when the metrics last actually moved;
// together they expose staleness. Like UsageEvent, tenant_id carries no FK in
// this database.
type PostAnalytics struct {
	bun.BaseModel `bun:"table:post_analytics_current,alias:pa" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks (no FK in the analytics DB)

	PostID             string                `bun:"post_id,notnull"                       json:"post_id"`
	PublisherPostID    string                `bun:"publisher_post_id,notnull"             json:"publisher_post_id"`
	Publisher          string                `bun:"publisher,notnull"                     json:"-"`
	Platform           string                `bun:"platform,nullzero"                     json:"-"`
	Title              string                `bun:"title,nullzero"                        json:"-"`
	PublishedAt        *time.Time            `bun:"published_at,nullzero"                 json:"-"`
	Impressions        int                   `bun:"impressions,notnull"                   json:"impressions"`
	Reach              int                   `bun:"reach,notnull"                         json:"reach"`
	Likes              int                   `bun:"likes,notnull"                         json:"likes"`
	Comments           int                   `bun:"comments,notnull"                      json:"comments"`
	Shares             int                   `bun:"shares,notnull"                        json:"shares"`
	Saves              int                   `bun:"saves,notnull"                         json:"saves"`
	Clicks             int                   `bun:"clicks,notnull"                        json:"clicks"`
	Views              int                   `bun:"views,notnull"                         json:"views"`
	EngagementRate     float64               `bun:"engagement_rate,notnull"               json:"engagement_rate"`
	PlatformAnalytics  PlatformAnalyticsList `bun:"platform_analytics,notnull,type:jsonb" json:"platform_analytics"`
	SyncStatus         string                `bun:"sync_status,nullzero"                  json:"sync_status"`
	MetricsLastUpdated *time.Time            `bun:"metrics_last_updated"                  json:"metrics_last_updated"`
	// FirstSeenAt is when the first snapshot for this post was recorded;
	// LastChangedAt when the metrics last moved; LastCheckedAt when the refresh
	// last looked (bumped every check). The handler exposes LastCheckedAt to
	// clients as last_refreshed_at (API compatibility).
	FirstSeenAt   time.Time `bun:"first_seen_at,notnull,default:current_timestamp"   json:"-"`
	LastChangedAt time.Time `bun:"last_changed_at,notnull,default:current_timestamp" json:"-"`
	LastCheckedAt time.Time `bun:"last_checked_at,notnull,default:current_timestamp" json:"-"`
}

// Metrics assembles the denormalised aggregate columns into the response
// block.
func (a *PostAnalytics) Metrics() PostAnalyticsMetrics {
	return PostAnalyticsMetrics{
		Impressions:    a.Impressions,
		Reach:          a.Reach,
		Likes:          a.Likes,
		Comments:       a.Comments,
		Shares:         a.Shares,
		Saves:          a.Saves,
		Clicks:         a.Clicks,
		Views:          a.Views,
		EngagementRate: a.EngagementRate,
	}
}

// MetricsKey is the change-detection fingerprint: two checks whose
// keys are equal represent identical engagement, so no new history snapshot is
// written. It covers the aggregate metrics plus sync_status (a status
// transition — e.g. error→synced — is a meaningful change even at equal
// numbers). Kept as a comparable struct so callers dedup with ==.
type MetricsKey struct {
	Impressions    int
	Reach          int
	Likes          int
	Comments       int
	Shares         int
	Saves          int
	Clicks         int
	Views          int
	EngagementRate float64
	SyncStatus     string
}

// MetricsKey returns the current row's change-detection fingerprint.
func (a *PostAnalytics) MetricsKey() MetricsKey {
	return MetricsKey{
		Impressions:    a.Impressions,
		Reach:          a.Reach,
		Likes:          a.Likes,
		Comments:       a.Comments,
		Shares:         a.Shares,
		Saves:          a.Saves,
		Clicks:         a.Clicks,
		Views:          a.Views,
		EngagementRate: a.EngagementRate,
		SyncStatus:     a.SyncStatus,
	}
}

// NewSnapshot builds an append-only history point from the current-state row,
// copying only the varying metric columns. id + occurredAt are set by
// the caller; tenant_id is stamped by the TenantScoped hook on insert.
func (a *PostAnalytics) NewSnapshot(id string, occurredAt time.Time) *PostAnalyticsSnapshot {
	return &PostAnalyticsSnapshot{
		ID:                 id,
		PostID:             a.PostID,
		Impressions:        a.Impressions,
		Reach:              a.Reach,
		Likes:              a.Likes,
		Comments:           a.Comments,
		Shares:             a.Shares,
		Saves:              a.Saves,
		Clicks:             a.Clicks,
		Views:              a.Views,
		EngagementRate:     a.EngagementRate,
		SyncStatus:         a.SyncStatus,
		MetricsLastUpdated: a.MetricsLastUpdated,
		OccurredAt:         occurredAt,
	}
}

// PostAnalyticsSnapshot is one point in a post's append-only engagement trend
// history. The refresh queue appends a row ONLY when the metrics
// change (dedup), so the series holds real movement rather than one row per
// tick. It lives in the isolated analytics DB (hypertable post_analytics_snapshots,
// partitioned on occurred_at, retention-pruned). No current endpoint reads it;
// it is the durable trend archive. Only the varying data lives here — the
// static post display fields stay on PostAnalytics (one row per post).
type PostAnalyticsSnapshot struct {
	bun.BaseModel `bun:"table:post_analytics_snapshots,alias:pas" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks (no FK in the analytics DB)

	ID                 string     `bun:"id,notnull"`
	PostID             string     `bun:"post_id,notnull"`
	Impressions        int        `bun:"impressions,notnull"`
	Reach              int        `bun:"reach,notnull"`
	Likes              int        `bun:"likes,notnull"`
	Comments           int        `bun:"comments,notnull"`
	Shares             int        `bun:"shares,notnull"`
	Saves              int        `bun:"saves,notnull"`
	Clicks             int        `bun:"clicks,notnull"`
	Views              int        `bun:"views,notnull"`
	EngagementRate     float64    `bun:"engagement_rate,notnull"`
	SyncStatus         string     `bun:"sync_status,nullzero"`
	MetricsLastUpdated *time.Time `bun:"metrics_last_updated"`
	// OccurredAt is the moment this change was recorded (the hypertable's
	// partition column).
	OccurredAt time.Time `bun:"occurred_at,notnull,default:current_timestamp"`
}
