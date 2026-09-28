package models

import (
	"time"

	"github.com/uptrace/bun"
)

// EmailKind separates essential transactional mail (welcome, password reset)
// from marketing mail (onboarding drip). It drives suppression semantics and
// whether an unsubscribe footer is required — see the send_email job.
type EmailKind string

const (
	// EmailKindTransactional is essential mail tied to a user action. It is
	// sent even to marketing-unsubscribed recipients; only an `all`-scope
	// suppression (hard bounce / complaint) blocks it.
	EmailKindTransactional EmailKind = "transactional"
	// EmailKindMarketing is promotional / lifecycle mail. It is blocked by any
	// suppression (marketing or all) and must carry an unsubscribe affordance.
	EmailKindMarketing EmailKind = "marketing"
)

// EmailTemplate is one editable email template, keyed by a stable id (CON-154
// §7). Rows are seeded on boot from embedded Maizzle-compiled defaults
// (upsert-if-absent); once present the DB row is authoritative and editable at
// runtime, so operator copy edits survive redeploys. Bodies may carry template
// variables — rendered with custom Go delimiters at send time (see src/email).
type EmailTemplate struct {
	bun.BaseModel `bun:"table:email_templates,alias:et" swaggerignore:"true"`

	Key     string    `bun:"key,pk"          json:"key"`
	Subject string    `bun:"subject,notnull" json:"subject"`
	HTML    string    `bun:"html,notnull"    json:"html"`
	Text    string    `bun:"text,notnull"    json:"text"`
	Kind    EmailKind `bun:"kind,notnull"    json:"kind"`
	// Variables documents the template's runtime placeholders: key = the
	// variable name as referenced via [[ .Key ]], value = a human explanation.
	// Code-owned metadata (the source of truth is the seeder's default set); the
	// boot seeder keeps it in sync while leaving operator-edited copy untouched.
	// It is NOT used at render time — rendering fills from the typed Data struct.
	Variables StringMap `bun:"variables,nullzero,notnull,type:jsonb,default:'{}'" json:"variables"`
	Version   int       `bun:"version,notnull,default:1"                          json:"version"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updated_at"`
}
