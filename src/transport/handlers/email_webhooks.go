package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// resendWebhookTolerance bounds how far a webhook timestamp may drift from now
// before it's rejected as stale (replay protection). Svix's own default.
const resendWebhookTolerance = 5 * time.Minute

var errInvalidSignature = errors.New("resend webhook: invalid signature")

// ResendWebhookHandler ingests Resend delivery events (CON-154 FR8, CON-298).
// Resend signs webhooks with Svix, so every request is signature-verified before
// any side effect. Hard bounces and complaints auto-suppress the address (scope
// all); every delivery/open/click/delay/bounce/complaint event is also persisted
// to the email_events timeline and folded into the email_logs rollup.
type ResendWebhookHandler struct {
	suppressions  repository.EmailSuppressionRepository
	logs          repository.EmailLogRepository
	events        repository.EmailEventRepository
	webhookSecret SecretResolver
}

// NewResendWebhookHandler constructs the webhook handler.
func NewResendWebhookHandler(suppressions repository.EmailSuppressionRepository, logs repository.EmailLogRepository, events repository.EmailEventRepository, webhookSecret SecretResolver) *ResendWebhookHandler {
	return &ResendWebhookHandler{suppressions: suppressions, logs: logs, events: events, webhookSecret: webhookSecret}
}

// Register mounts the public (signature-gated) webhook route.
func (h *ResendWebhookHandler) Register(app *fiber.App) {
	app.Post("/api/webhooks/resend", h.Handle)
}

// resendEvent is the subset of the Resend/Svix event envelope we act on. The
// top-level created_at is the event time (RFC3339); we record it as the event's
// occurred_at, falling back to now if absent.
type resendEvent struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		EmailID string   `json:"email_id"`
		To      []string `json:"to"`
	} `json:"data"`
}

// Handle godoc
// @Summary      Resend delivery webhook
// @Description  Signature-verified ingest of Resend events. Bounces/complaints
// @Description  suppress the address; delivery events update the send audit.
// @Tags         email
// @Accept       json
// @Param        svix-id         header  string  true  "Svix message id"
// @Param        svix-timestamp  header  string  true  "Svix timestamp"
// @Param        svix-signature  header  string  true  "Svix signature"
// @Success      200  "Acknowledged"
// @Failure      401  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/webhooks/resend [post]
func (h *ResendWebhookHandler) Handle(c *fiber.Ctx) error {
	secret, err := h.webhookSecret(c.Context())
	if err != nil {
		return err // secret-read failure → 500
	}
	if secret == "" {
		// Fail closed: without a secret we can't verify, so refuse rather than
		// trust an unauthenticated caller.
		return fiber.NewError(fiber.StatusServiceUnavailable, "resend webhook not configured")
	}

	body := c.Body()
	if err := verifySvixSignature(secret, c.Get("svix-id"), c.Get("svix-timestamp"), c.Get("svix-signature"), body, time.Now()); err != nil {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid signature")
	}

	var evt resendEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid payload")
	}

	ctx := c.Context()
	// The event's occurred_at; fall back to now when the envelope omits it.
	occurredAt := evt.CreatedAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	// The svix-id header is the idempotency key for a redelivered event; the
	// signature check above guarantees it is non-empty.
	svixID := c.Get("svix-id")

	// errors.Join evaluates both calls (Go evaluates args before the call), so
	// every address + both side effects are attempted even if one fails; a
	// non-nil result means at least one did.
	var sideErr error
	switch evt.Type {
	case "email.bounced":
		sideErr = errors.Join(
			h.suppressAll(ctx, evt.Data.To, models.EmailSuppressionReasonBounce),
			h.recordEvent(ctx, evt.Data.EmailID, svixID, models.EmailEventBounced, occurredAt),
		)
	case "email.complained":
		sideErr = errors.Join(
			h.suppressAll(ctx, evt.Data.To, models.EmailSuppressionReasonComplaint),
			h.recordEvent(ctx, evt.Data.EmailID, svixID, models.EmailEventComplained, occurredAt),
		)
	case "email.delivered":
		sideErr = h.recordEvent(ctx, evt.Data.EmailID, svixID, models.EmailEventDelivered, occurredAt)
	case "email.opened":
		sideErr = h.recordEvent(ctx, evt.Data.EmailID, svixID, models.EmailEventOpened, occurredAt)
	case "email.clicked":
		sideErr = h.recordEvent(ctx, evt.Data.EmailID, svixID, models.EmailEventClicked, occurredAt)
	case "email.delivery_delayed":
		sideErr = h.recordEvent(ctx, evt.Data.EmailID, svixID, models.EmailEventDelayed, occurredAt)
	default:
		// Any other event type: acknowledged as a no-op so Resend stops retrying.
		slog.InfoContext(ctx, "resend webhook ignored", logging.AttrComponent, "handlers.resend_webhook", "type", evt.Type)
	}

	// A failed side effect must NOT be acked 2xx: return 5xx so Resend retries
	// the event rather than dropping a bounce/complaint suppression on the floor.
	// The side effects are idempotent (upsert / status update), so a retry is safe.
	if sideErr != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "webhook side effect failed")
	}
	return c.SendStatus(fiber.StatusOK)
}

// suppressAll adds an all-scope suppression for each address. It is best-effort
// ACROSS addresses (every address is attempted even if one fails) but returns
// the first error so the caller can decline to ack — a dropped bounce/complaint
// suppression must not be lost to a 2xx.
func (h *ResendWebhookHandler) suppressAll(ctx context.Context, addrs []string, reason models.EmailSuppressionReason) error {
	var firstErr error
	for _, a := range addrs {
		if a == "" {
			continue
		}
		id, err := models.NewID()
		if err != nil {
			slog.WarnContext(ctx, "suppression id gen failed", logging.AttrComponent, "handlers.resend_webhook", logging.AttrError, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := h.suppressions.Upsert(ctx, &models.EmailSuppression{
			ID:     id,
			Email:  repository.NormalizeEmail(a),
			Scope:  models.EmailSuppressionScopeAll,
			Reason: reason,
			Source: models.EmailSuppressionSourceWebhook,
		}); err != nil {
			slog.WarnContext(ctx, "suppression upsert failed", logging.AttrComponent, "handlers.resend_webhook", logging.AttrError, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// recordEvent persists one delivery event and refreshes the parent log's rollup
// (CON-298). It resolves the log by the Resend message id first: an unknown id is
// not an error (returns nil, so a webhook for a pruned/foreign message can't
// cause a retry storm); only a real persistence failure is returned, blocking
// the ack so Resend retries. Ingestion is idempotent on svix_id.
func (h *ResendWebhookHandler) recordEvent(ctx context.Context, providerMessageID, svixID string, typ models.EmailEventType, occurredAt time.Time) error {
	if providerMessageID == "" || h.logs == nil || h.events == nil {
		return nil
	}
	log, err := h.logs.GetByProviderMessageID(ctx, providerMessageID)
	if err != nil {
		slog.WarnContext(ctx, "email_log lookup failed", logging.AttrComponent, "handlers.resend_webhook", logging.AttrError, err)
		return err
	}
	if log == nil {
		return nil // unknown message id: ack, don't retry
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	if err := h.events.Record(ctx, &models.EmailEvent{
		ID:                id,
		EmailLogID:        log.ID,
		ProviderMessageID: providerMessageID,
		Type:              typ,
		OccurredAt:        occurredAt,
		SvixID:            svixID,
	}); err != nil {
		slog.WarnContext(ctx, "email_event record failed", logging.AttrComponent, "handlers.resend_webhook", logging.AttrError, err)
		return err
	}
	return nil
}

// verifySvixSignature validates a Svix-signed webhook (the scheme Resend uses).
// signedContent = "<id>.<timestamp>.<body>"; the expected signature is the
// base64 HMAC-SHA256 of that content keyed by the (base64-decoded) secret. The
// svix-signature header is a space-separated list of "v1,<sig>" entries; a match
// against any is sufficient. The timestamp must be within tolerance of now.
func verifySvixSignature(secret, id, timestamp, signatureHeader string, body []byte, now time.Time) error {
	if id == "" || timestamp == "" || signatureHeader == "" {
		return errInvalidSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errInvalidSignature
	}
	if d := now.Unix() - ts; d > int64(resendWebhookTolerance/time.Second) || d < -int64(resendWebhookTolerance/time.Second) {
		return errInvalidSignature
	}

	raw := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Some setups store the raw secret without base64; fall back to the
		// prefix-stripped bytes (consistent with the decode path above).
		key = []byte(raw)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	for _, part := range strings.Fields(signatureHeader) {
		comma := strings.IndexByte(part, ',')
		if comma < 0 {
			continue
		}
		if hmac.Equal([]byte(part[comma+1:]), []byte(expected)) {
			return nil
		}
	}
	return errInvalidSignature
}
