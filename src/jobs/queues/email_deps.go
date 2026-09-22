package queues

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

// EmailDeps bundles everything the send_email + cleanup_email_logs workers need
// (CON-154). Constructed once at boot in server.go. A nil Sender means no
// Resend key is wired, so the send job degrades to skipped_disabled rather than
// erroring — mirroring the nil-client pattern used for PDF/Zernio.
type EmailDeps struct {
	Sender       email.Sender
	Templates    repository.EmailTemplateRepository
	Suppressions repository.EmailSuppressionRepository
	Logs         repository.EmailLogRepository
	Users        repository.UserRepository

	// Bodies persists the rendered body at send so the operator Emails tab
	// (CON-192) renders it even after the Resend message ages out of retention
	// (CON-306). nil = bodies not stored; the live Resend fetch still applies.
	Bodies repository.EmailBodyRepository

	// From / ReplyTo are the message envelope; AppBaseURL builds absolute CTA
	// links in rendered mail. LinkBaseURL is the base for public unsubscribe
	// links (must resolve to the API host; CON-155) — set to EMAIL_LINK_BASE_URL
	// or, when unset, AppBaseURL.
	From        string
	ReplyTo     string
	AppBaseURL  string
	LinkBaseURL string

	// LinkSecret resolves the HMAC key that signs unsubscribe links, per call
	// (so a rotation takes effect with no restart). A read error is transient;
	// an empty value means marketing mail can't build an unsubscribe link.
	LinkSecret func(ctx context.Context) (string, error)

	// Retention is the cleanup_email_logs window; 0 disables the sweep.
	Retention time.Duration

	// ActivityRecorder emits CON-125 email-category activity events into
	// tenant_activity_events — one per terminal send outcome (sent / failed /
	// skipped), mirroring the email_logs row. nil = no-op (analytics disabled).
	ActivityRecorder *activity.Recorder
}

// unsubscribeURL builds the absolute one-click unsubscribe link for toEmail,
// signed with secret. The address is normalised so the token round-trips
// exactly what the suppression list keys on.
func (d EmailDeps) unsubscribeURL(secret, toEmail string) string {
	token := email.SignUnsubscribe(secret, repository.NormalizeEmail(toEmail), time.Now().UTC())
	base := strings.TrimRight(d.LinkBaseURL, "/")
	return base + "/api/email/unsubscribe?token=" + url.QueryEscape(token)
}
