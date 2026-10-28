package lemonsqueezy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// VerifyWebhook implements billing.Provider: X-Signature is the hex
// HMAC-SHA256 of the raw body under the webhook signing secret.
func (c *Client) VerifyWebhook(ctx context.Context, signature string, body []byte) error {
	secret, err := c.webhookSecret(ctx)
	if err != nil {
		return fmt.Errorf("lemonsqueezy: resolve webhook secret: %w", err)
	}
	if secret == "" {
		return billing.ErrNotConfigured
	}
	return verifySignature(secret, signature, body)
}

func verifySignature(secret, signature string, body []byte) error {
	got, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(got) == 0 {
		return billing.ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return billing.ErrInvalidSignature
	}
	return nil
}

// Sign computes the X-Signature for a body. Used by tests and fixtures.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type webhookEnvelope struct {
	Meta struct {
		EventName  string          `json:"event_name"`
		TestMode   *bool           `json:"test_mode"`
		CustomData json.RawMessage `json:"custom_data"`
	} `json:"meta"`
	Data resource `json:"data"`
}

// ParseWebhook implements billing.Provider.
func (c *Client) ParseWebhook(body []byte) (billing.Event, error) {
	return parseWebhook(body)
}

func parseWebhook(body []byte) (billing.Event, error) {
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return billing.Event{}, fmt.Errorf("lemonsqueezy: decode webhook: %w", err)
	}
	if env.Meta.EventName == "" {
		return billing.Event{}, fmt.Errorf("lemonsqueezy: webhook has no event_name")
	}
	ev := billing.Event{
		Name:       env.Meta.EventName,
		Kind:       billing.EventOther,
		ObjectID:   string(env.Data.ID),
		CustomData: customData(env.Meta.CustomData),
	}
	var mode struct {
		TestMode bool `json:"test_mode"`
	}
	_ = json.Unmarshal(env.Data.Attributes, &mode)
	ev.TestMode = mode.TestMode
	if env.Meta.TestMode != nil {
		ev.TestMode = *env.Meta.TestMode
	}

	switch {
	case strings.HasPrefix(ev.Name, "subscription_payment_"):
		var a struct {
			SubscriptionID flexString `json:"subscription_id"`
			Total          int64      `json:"total"`
			Status         string     `json:"status"`
		}
		if err := json.Unmarshal(env.Data.Attributes, &a); err != nil {
			return billing.Event{}, fmt.Errorf("lemonsqueezy: decode invoice: %w", err)
		}
		ev.Kind = billing.EventPayment
		ev.PaymentSubscriptionID = string(a.SubscriptionID)
		ev.PaymentAmountMinor = a.Total
		ev.PaymentStatus = a.Status
		ev.Payments = webhookPayments(billing.PaymentSourceSubscriptionInvoice, ev, env.Data.Attributes)
	case strings.HasPrefix(ev.Name, "order_"):
		ev.Payments = webhookPayments(billing.PaymentSourceOrder, ev, env.Data.Attributes)
	case strings.HasPrefix(ev.Name, "subscription_"):
		var a subscriptionAttrs
		if err := json.Unmarshal(env.Data.Attributes, &a); err != nil {
			return billing.Event{}, fmt.Errorf("lemonsqueezy: decode subscription: %w", err)
		}
		sub := a.toSubscription(string(env.Data.ID))
		// A webhook's portal link is not meant to be handed out; the portal
		// endpoint always reads a fresh one.
		sub.PortalURL = ""
		ev.Kind = billing.EventSubscription
		ev.Subscription = &sub
	}
	return ev, nil
}

// webhookPayments reads the payments an event reports. One that does not
// decode is logged and skipped, so it never blocks the rest of the event; the
// payments sync reads it again from the API.
func webhookPayments(source string, ev billing.Event, attrs json.RawMessage) []billing.Payment {
	payments, err := decodePayments(source, ev.ObjectID, attrs)
	if err != nil {
		slog.Warn("lemonsqueezy: webhook payment skipped", logging.AttrComponent, "billing.lemonsqueezy", "event", ev.Name, logging.AttrError, err)
		return nil
	}
	return payments
}

// customData flattens meta.custom_data to strings; numbers and booleans are
// kept in their JSON text form.
func customData(raw json.RawMessage) map[string]string {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
			continue
		}
		out[k] = string(v)
	}
	return out
}
