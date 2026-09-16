package models

import (
	"time"

	"github.com/uptrace/bun"
)

// EmailLogStatus is the terminal (or terminal-ish) outcome recorded for one
// send attempt. Delivery events arriving later via the Resend webhook update
// the same row in place by provider_message_id (CON-154 FR8).
type EmailLogStatus string

const (
	EmailLogQueued            EmailLogStatus = "queued"
	EmailLogSent              EmailLogStatus = "sent"
	EmailLogFailed            EmailLogStatus = "failed"
	EmailLogSkippedSuppressed EmailLogStatus = "skipped_suppressed"
	EmailLogSkippedDisabled   EmailLogStatus = "skipped_disabled"
	// EmailLogDelivered / EmailLogOpened / EmailLogClicked are the positive
	// delivery-lifecycle states written by the Resend webhook (CON-298) as
	// email.delivered / email.opened / email.clicked events arrive.
	EmailLogDelivered EmailLogStatus = "delivered"
	EmailLogOpened    EmailLogStatus = "opened"
	EmailLogClicked   EmailLogStatus = "clicked"
	// EmailLogBounced / EmailLogComplained are written by the webhook when
	// Resend reports a hard bounce or a spam complaint against a prior send.
	EmailLogBounced    EmailLogStatus = "bounced"
	EmailLogComplained EmailLogStatus = "complained"
)

// Rank orders the send lifecycle so an out-of-order or redelivered webhook can
// never regress a row to a less-advanced state (CON-298). The positive
// progression is queued < sent < delivered < opened < clicked. The skipped_*
// and terminal-negative states (failed/bounced/complained) rank ABOVE the
// positive progression so they stick — e.g. a spam complaint that arrives after
// an open still wins, and a late "delivered" can't overwrite a "bounced". A row
// only advances to a status whose Rank exceeds its current one.
func (s EmailLogStatus) Rank() int {
	switch s {
	case EmailLogQueued:
		return 1
	case EmailLogSent:
		return 2
	case EmailLogDelivered:
		return 3
	case EmailLogOpened:
		return 4
	case EmailLogClicked:
		return 5
	case EmailLogSkippedDisabled, EmailLogSkippedSuppressed:
		return 6
	case EmailLogFailed:
		return 7
	case EmailLogBounced, EmailLogComplained:
		return 8
	default:
		return 0
	}
}

// ProviderResend is the only mail provider today (CON-154). Recorded on every
// row so a future second provider stays distinguishable in the audit trail.
const ProviderResend = "resend"

// EmailLog is one entry in the append-only send audit (CON-154 §7), mirroring
// PostLog. tenant_id / user_id are nullable because some mail (future
// system/ops notices) has no owning tenant or user. idempotency_key is nullable
// so the partial unique index only constrains rows that carry one.
type EmailLog struct {
	bun.BaseModel `bun:"table:email_logs,alias:el" swaggerignore:"true"`

	ID                string         `bun:"id,pk"                                        json:"id"`
	TenantID          string         `bun:"tenant_id,nullzero"                           json:"tenant_id,omitempty"`
	UserID            string         `bun:"user_id,nullzero"                             json:"user_id,omitempty"`
	TemplateID        string         `bun:"template_id,notnull"                          json:"template_id"`
	Kind              EmailKind      `bun:"kind,notnull"                                 json:"kind"`
	ToEmail           string         `bun:"to_email,notnull"                             json:"to_email"`
	Status            EmailLogStatus `bun:"status,notnull"                               json:"status"`
	Provider          string         `bun:"provider,notnull,default:'resend'"            json:"provider"`
	ProviderMessageID string         `bun:"provider_message_id,nullzero"                 json:"provider_message_id,omitempty"`
	IdempotencyKey    string         `bun:"idempotency_key,nullzero"                     json:"idempotency_key,omitempty"`
	Error             string         `bun:"error,nullzero"                               json:"error,omitempty"`

	// CON-298 engagement rollup, denormalised from email_events for cheap list
	// queries. LastEvent/LastEventAt track the most recent event of any type;
	// DeliveredAt/FirstOpenedAt are first-occurrence timestamps; the counts are
	// totals. All recomputed from email_events on every webhook.
	LastEvent     string    `bun:"last_event,nullzero"      json:"last_event,omitempty"`
	LastEventAt   time.Time `bun:"last_event_at,nullzero"   json:"last_event_at,omitempty"`
	DeliveredAt   time.Time `bun:"delivered_at,nullzero"    json:"delivered_at,omitempty"`
	FirstOpenedAt time.Time `bun:"first_opened_at,nullzero" json:"first_opened_at,omitempty"`
	OpensCount    int       `bun:"opens_count,notnull,default:0"  json:"opens_count"`
	ClicksCount   int       `bun:"clicks_count,notnull,default:0" json:"clicks_count"`

	CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updated_at"`
}
