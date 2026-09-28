package models

import (
	"time"

	"github.com/uptrace/bun"
)

// EmailSuppressionScope bounds what an entry blocks. `marketing` (an
// unsubscribe) blocks only marketing mail; `all` (a hard bounce or spam
// complaint) blocks every kind, transactional included — the address is
// dead or hostile.
type EmailSuppressionScope string

const (
	EmailSuppressionScopeMarketing EmailSuppressionScope = "marketing"
	EmailSuppressionScopeAll       EmailSuppressionScope = "all"
)

// EmailSuppressionReason records why the entry exists (audit / debugging).
type EmailSuppressionReason string

const (
	EmailSuppressionReasonUnsubscribe EmailSuppressionReason = "unsubscribe"
	EmailSuppressionReasonBounce      EmailSuppressionReason = "bounce"
	EmailSuppressionReasonComplaint   EmailSuppressionReason = "complaint"
	EmailSuppressionReasonManual      EmailSuppressionReason = "manual"
)

// EmailSuppressionSource records who added the entry.
type EmailSuppressionSource string

const (
	EmailSuppressionSourceWebhook EmailSuppressionSource = "webhook"
	EmailSuppressionSourceUser    EmailSuppressionSource = "user"
	EmailSuppressionSourceOps     EmailSuppressionSource = "ops"
)

// EmailSuppression is one "do not mail" entry, keyed by email address (the
// global identifier — email is unique across tenants) plus scope. Unique on
// (email, scope) so an unsubscribe and a later hard bounce coexist as two rows.
type EmailSuppression struct {
	bun.BaseModel `bun:"table:email_suppressions,alias:es" swaggerignore:"true"`

	ID        string                 `bun:"id,pk"                                        json:"id"`
	Email     string                 `bun:"email,notnull"                                json:"email"`
	Scope     EmailSuppressionScope  `bun:"scope,notnull"                                json:"scope"`
	Reason    EmailSuppressionReason `bun:"reason,notnull"                               json:"reason"`
	Source    EmailSuppressionSource `bun:"source,notnull"                               json:"source"`
	CreatedAt time.Time              `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"created_at"`
}
