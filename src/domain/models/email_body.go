package models

import (
	"time"

	"github.com/uptrace/bun"
)

// EmailBody is the rendered content of one sent email, persisted at send time so
// the operator Emails tab (CON-192) can show the body even after the Resend
// message ages out of retention or the Resend key is unset (CON-306). It is a
// 1:1 side table of email_logs — kept out of the hot ListTenantEmails query —
// and cascade-deleted with its parent log, so the existing email_logs retention
// sweep ages it out on OUR schedule (EMAIL_LOG_RETENTION_DAYS), decoupled from
// Resend's retention. Reverses CON-298's live-fetch-only decision; the live
// Resend fetch remains a fallback for rows sent before this shipped.
//
// cc/bcc are omitted deliberately: the send path only sets a single To, so
// they'd always be empty; the live-fetch fallback still fills them when Resend
// has them. Fields are notnull (empty string is a legitimate stored value, e.g.
// a text-only or html-only template), matching the DEFAULT ” columns.
type EmailBody struct {
	bun.BaseModel `bun:"table:email_bodies,alias:eb" swaggerignore:"true"`

	EmailLogID string    `bun:"email_log_id,pk"                                       json:"email_log_id"`
	Subject    string    `bun:"subject,notnull"                                      json:"subject"`
	HTML       string    `bun:"html,notnull"                                         json:"html"`
	Text       string    `bun:"text,notnull"                                         json:"text"`
	From       string    `bun:"from_addr,notnull"                                    json:"from"`
	ReplyTo    string    `bun:"reply_to,notnull"                                     json:"reply_to"`
	CreatedAt  time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"created_at"`
}
