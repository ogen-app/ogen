package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AnnouncementStatus is the lifecycle of an operator-authored announcement
// (CON-230): a stable, closed set enforced by a DB CHECK and validated in Go.
//
//	draft     — authored, not yet delivered to any tenant.
//	published — live: delivered to matching tenants within its showing window.
//	archived  — retired from delivery; retained (with its stats) for history.
type AnnouncementStatus string

const (
	AnnouncementStatusDraft     AnnouncementStatus = "draft"
	AnnouncementStatusPublished AnnouncementStatus = "published"
	AnnouncementStatusArchived  AnnouncementStatus = "archived"
)

// Valid reports whether s is a known status.
func (s AnnouncementStatus) Valid() bool {
	switch s {
	case AnnouncementStatusDraft, AnnouncementStatusPublished, AnnouncementStatusArchived:
		return true
	default:
		return false
	}
}

// Announcement is one operator-authored informational banner (CON-230), shown to
// tenants over /api/announcements and authored by Harbor over the internal gRPC
// surface. Like Tenant / TenantTier / TenantGroup it is a GLOBAL operator table —
// NOT TenantScoped: a single row is shown to many tenants, so it carries no
// tenant_id. Targeting (all / by tier / by group) lives in the two join tables;
// per-user engagement lives in AnnouncementInteraction.
//
// The optional single CTA is a (label, url) pair: both set = a button, both
// empty = an info-only banner (dismiss-only). Impressions are deliberately NOT
// tracked (CON-230 decision 4) — only clicks and dismissals.
type Announcement struct {
	bun.BaseModel `bun:"table:announcements,alias:an" swaggerignore:"true"`

	ID          string             `bun:"id,pk"                                        json:"id"`
	Title       string             `bun:"title,notnull"                                json:"title"`
	Body        string             `bun:"body,notnull"                                 json:"body"`
	ImageURL    string             `bun:"image_url,notnull,default:''"                 json:"image_url,omitempty"`
	ImageAlt    string             `bun:"image_alt,notnull,default:''"                 json:"image_alt,omitempty"`
	CTALabel    string             `bun:"cta_label,notnull,default:''"                 json:"cta_label,omitempty"`
	CTAURL      string             `bun:"cta_url,notnull,default:''"                   json:"cta_url,omitempty"`
	TargetAll   bool               `bun:"target_all,notnull,default:false"             json:"target_all"`
	Status      AnnouncementStatus `bun:"status,notnull,default:'draft'"               json:"status"`
	StartsAt    *time.Time         `bun:"starts_at,nullzero"                           json:"starts_at,omitempty"`
	EndsAt      *time.Time         `bun:"ends_at,nullzero"                             json:"ends_at,omitempty"`
	PublishedAt *time.Time         `bun:"published_at,nullzero"                        json:"published_at,omitempty"`
	CreatedAt   time.Time          `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt   time.Time          `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// TargetGroupIDs / TargetTierIDs are hydrated from the join tables on the
	// operator read path (Harbor gRPC), never scanned from the announcements
	// table (bun:"-"). Empty when the announcement is target_all or unscoped on
	// that dimension.
	TargetGroupIDs []string `bun:"-" json:"target_group_ids,omitempty"`
	TargetTierIDs  []string `bun:"-" json:"target_tier_ids,omitempty"`
}

// AnnouncementTargetGroup binds an announcement to a tenant group it targets
// (CON-230). Composite PK (announcement_id, group_id). Global table.
type AnnouncementTargetGroup struct {
	bun.BaseModel `bun:"table:announcement_target_groups,alias:atg" swaggerignore:"true"`

	AnnouncementID string `bun:"announcement_id,pk" json:"announcement_id"`
	GroupID        string `bun:"group_id,pk"        json:"group_id"`
}

// AnnouncementTargetTier binds an announcement to a tenant tier it targets
// (CON-230). Composite PK (announcement_id, tier_id). Global table.
type AnnouncementTargetTier struct {
	bun.BaseModel `bun:"table:announcement_target_tiers,alias:att" swaggerignore:"true"`

	AnnouncementID string `bun:"announcement_id,pk" json:"announcement_id"`
	TierID         string `bun:"tier_id,pk"         json:"tier_id"`
}

// AnnouncementInteraction is one user's engagement with one announcement
// (CON-230): a single row per (announcement, user), upserted on first click /
// dismiss. It carries a denormalised tenant_id so the operator stats roll up to
// per-tenant counts. NOT TenantScoped — the operator stats reads are
// cross-tenant aggregates, so it carries explicit user_id + tenant_id columns
// and stamps them from the request session rather than the tenant hook.
type AnnouncementInteraction struct {
	bun.BaseModel `bun:"table:announcement_interactions,alias:ai" swaggerignore:"true"`

	ID             string     `bun:"id,pk"                                        json:"id"`
	AnnouncementID string     `bun:"announcement_id,notnull"                      json:"announcement_id"`
	UserID         string     `bun:"user_id,notnull"                              json:"user_id"`
	TenantID       string     `bun:"tenant_id,notnull"                            json:"tenant_id"`
	ClickedAt      *time.Time `bun:"clicked_at,nullzero"                          json:"clicked_at,omitempty"`
	DismissedAt    *time.Time `bun:"dismissed_at,nullzero"                        json:"dismissed_at,omitempty"`
	CreatedAt      time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt      time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// AnnouncementStats is the operator-facing engagement rollup for one
// announcement (CON-230), computed over announcement_interactions plus the
// eligible-audience denominator from the targeting predicate. Because
// impressions are not tracked, the denominator is the ELIGIBLE audience (tenants
// / users the announcement targets among active tenants), not the viewed set.
type AnnouncementStats struct {
	UniqueUsersClicked     int `json:"unique_users_clicked"`
	UniqueTenantsClicked   int `json:"unique_tenants_clicked"`
	UniqueUsersDismissed   int `json:"unique_users_dismissed"`
	UniqueTenantsDismissed int `json:"unique_tenants_dismissed"`
	EligibleTenants        int `json:"eligible_tenants"`
	EligibleUsers          int `json:"eligible_users"`
}
