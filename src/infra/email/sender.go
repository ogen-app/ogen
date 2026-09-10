// Package email defines the provider-agnostic mail contract for Ogen's
// transactional + marketing email subsystem (CON-154): the Sender interface,
// the fully-rendered Message it transmits, and the retry classification the
// send job relies on. The Resend implementation lives in the resend subpackage;
// template rendering + boot seeding live in the templates subpackage; the
// unsubscribe-token HMAC lives alongside in this package (token.go).
package email

import (
	"context"
	"errors"
	"fmt"
)

// Message is a single outbound email, fully rendered and ready to transmit. The
// send job builds it — subject/html/text from the stored template, headers for
// List-Unsubscribe, an idempotency key — and the Sender just puts it on the
// wire. It carries no notion of transactional vs marketing: suppression gating
// and the unsubscribe affordance are resolved upstream in the job.
type Message struct {
	From           string
	ReplyTo        string
	To             string
	Subject        string
	HTML           string
	Text           string
	Headers        map[string]string
	IdempotencyKey string
}

// Sender transmits one Message via a mail provider and returns the provider's
// message id (empty when the provider accepted the send but returned no id).
// Failures are returned as *SendError so the send job can tell a retryable
// transient fault (network, 5xx, 429) from a terminal one (4xx validation).
type Sender interface {
	Send(ctx context.Context, msg Message) (providerMessageID string, err error)
}

// SendError wraps a provider failure with a retry classification. Transient
// faults should be returned to River (retry with backoff); terminal faults
// should be logged as failed and swallowed (no retry — a malformed request or
// rejected recipient won't succeed on a re-attempt).
type SendError struct {
	Transient bool
	// Disabled marks the "no API key configured" case so the send job can log
	// skipped_disabled rather than failed — the subsystem is intentionally off,
	// not broken. Always terminal (never transient).
	Disabled bool
	Status   int // provider HTTP status when applicable, else 0
	Message  string
}

// IsDisabled reports whether err indicates the sender is unconfigured (no key).
func IsDisabled(err error) bool {
	se, ok := errors.AsType[*SendError](err)
	return ok && se.Disabled
}

func (e *SendError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("email: send failed (HTTP %d): %s", e.Status, e.Message)
	}
	return "email: send failed: " + e.Message
}

// IsTransient reports whether err from Sender.Send is worth retrying. A nil
// error is never transient. An unclassified (non-*SendError) error is treated
// as transient — better to retry than to silently drop mail on an unknown
// fault.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if se, ok := errors.AsType[*SendError](err); ok {
		return se.Transient
	}
	return true
}
