package models

import (
	"time"

	"github.com/uptrace/bun"
)

// EmailEventType is one Resend delivery-lifecycle event persisted to the
// append-only email_events timeline (CON-298).
type EmailEventType string

const (
	EmailEventDelivered  EmailEventType = "delivered"
	EmailEventOpened     EmailEventType = "opened"
	EmailEventClicked    EmailEventType = "clicked"
	EmailEventDelayed    EmailEventType = "delivery_delayed"
	EmailEventBounced    EmailEventType = "bounced"
	EmailEventComplained EmailEventType = "complained"
)

// StatusForEvent maps an event type to the EmailLogStatus it implies, or the
// empty status when the event carries no lifecycle advancement (delivery_delayed
// is informational). The caller advances a log's status only when the implied
// status out-ranks the current one (see EmailLogStatus.Rank).
func StatusForEvent(t EmailEventType) EmailLogStatus {
	switch t {
	case EmailEventDelivered:
		return EmailLogDelivered
	case EmailEventOpened:
		return EmailLogOpened
	case EmailEventClicked:
		return EmailLogClicked
	case EmailEventBounced:
		return EmailLogBounced
	case EmailEventComplained:
		return EmailLogComplained
	default:
		return ""
	}
}

// EmailEvent is one persisted Resend webhook event (CON-298). It is append-only
// and cascade-deleted with its parent email_logs row. SvixID (the svix-id
// header) is globally unique and stable across a redelivery of the same event,
// so a UNIQUE index on it makes ingestion idempotent. ProviderMessageID is
// denormalised alongside email_log_id for convenience/debugging.
type EmailEvent struct {
	bun.BaseModel `bun:"table:email_events,alias:ee" swaggerignore:"true"`

	ID                string         `bun:"id,pk"                       json:"id"`
	EmailLogID        string         `bun:"email_log_id,notnull"        json:"email_log_id"`
	ProviderMessageID string         `bun:"provider_message_id,nullzero" json:"provider_message_id,omitempty"`
	Type              EmailEventType `bun:"type,notnull"                json:"type"`
	OccurredAt        time.Time      `bun:"occurred_at,notnull"         json:"occurred_at"`
	SvixID            string         `bun:"svix_id,notnull"             json:"svix_id"`
	Metadata          map[string]any `bun:"metadata,type:jsonb,nullzero" json:"metadata,omitempty"`
	CreatedAt         time.Time      `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"created_at"`
}
