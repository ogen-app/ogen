package polar

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// Webhooks follow the Standard Webhooks spec: webhook-signature carries one
// or more "v1,<base64 HMAC-SHA256>" entries over "<id>.<timestamp>.<body>".

// webhookTolerance bounds how far a delivery's timestamp may be from now, so
// a captured delivery cannot be replayed later.
const webhookTolerance = 5 * time.Minute

// VerifyWebhook implements billing.Provider.
func (c *Client) VerifyWebhook(ctx context.Context, h billing.WebhookHeaders, body []byte) error {
	if c.webhookSecret == nil {
		return billing.ErrNotConfigured
	}
	secret, err := c.webhookSecret(ctx)
	if err != nil {
		return fmt.Errorf("polar: resolve webhook secret: %w", err)
	}
	if secret == "" {
		return billing.ErrNotConfigured
	}
	return verify(secret, h, body, c.now())
}

func verify(secret string, h billing.WebhookHeaders, body []byte, now time.Time) error {
	if h.ID == "" || h.Timestamp == "" || h.Signature == "" {
		return billing.ErrInvalidSignature
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(h.Timestamp), 10, 64)
	if err != nil {
		return billing.ErrInvalidSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > webhookTolerance || d < -webhookTolerance {
		return billing.ErrInvalidSignature
	}
	for _, key := range signingKeys(secret) {
		want := sign(key, h.ID, h.Timestamp, body)
		for entry := range strings.FieldsSeq(h.Signature) {
			version, sig, ok := strings.Cut(entry, ",")
			if !ok || version != "v1" {
				continue
			}
			got, err := base64.StdEncoding.DecodeString(sig)
			if err == nil && hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return billing.ErrInvalidSignature
}

// signingKeys are the HMAC keys a secret may stand for. A Standard Webhooks
// secret is "whsec_" plus the base64 key; Polar's own secrets are used as
// raw bytes (its SDK base64-encodes them before handing them to Standard
// Webhooks). Both are tried, so either kind of endpoint secret works (spike:
// which one a new endpoint gets).
func signingKeys(secret string) [][]byte {
	secret = strings.TrimSpace(secret)
	keys := [][]byte{[]byte(secret)}
	if rest, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if k, err := base64.StdEncoding.DecodeString(rest); err == nil && len(k) > 0 {
			keys = append(keys, k)
		}
	}
	return keys
}

func sign(key []byte, id, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return mac.Sum(nil)
}

// Sign returns the Standard Webhooks headers for a body under a raw secret.
// Used by tests and fixtures.
func Sign(secret, id string, at time.Time, body []byte) billing.WebhookHeaders {
	ts := strconv.FormatInt(at.Unix(), 10)
	sig := base64.StdEncoding.EncodeToString(sign([]byte(secret), id, ts, body))
	return billing.WebhookHeaders{ID: id, Timestamp: ts, Signature: "v1," + sig}
}

type webhookEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ParseWebhook implements billing.Provider. Every delivery to this endpoint
// belongs to the client's own mode: the sandbox and production send from
// separate organizations to separate endpoints.
func (c *Client) ParseWebhook(h billing.WebhookHeaders, body []byte) (billing.Event, error) {
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return billing.Event{}, fmt.Errorf("polar: decode webhook: %w", err)
	}
	if env.Type == "" {
		return billing.Event{}, fmt.Errorf("polar: webhook has no type")
	}
	ev := billing.Event{ID: h.ID, Name: env.Type, Kind: billing.EventOther, TestMode: c.testMode}
	var err error
	switch {
	case strings.HasPrefix(env.Type, "subscription."):
		err = c.subscriptionEvent(&ev, env.Data)
	case strings.HasPrefix(env.Type, "order."):
		err = c.orderEvent(&ev, env.Data)
	case strings.HasPrefix(env.Type, "refund."):
		err = c.refundEvent(&ev, env.Data)
	default:
		var obj struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(env.Data, &obj)
		ev.ObjectID = obj.ID
	}
	return ev, err
}

func (c *Client) subscriptionEvent(ev *billing.Event, data json.RawMessage) error {
	sub, err := c.decodeSubscription(data)
	if err != nil {
		return err
	}
	ev.Kind, ev.ObjectID, ev.Subscription = billing.EventSubscription, sub.ID, &sub
	ev.Metadata, ev.CustomerExternalID = sub.Metadata, sub.CustomerExternalID
	return nil
}

func (c *Client) orderEvent(ev *billing.Event, data json.RawMessage) error {
	o, err := decodeOrder(data)
	if err != nil {
		return err
	}
	ev.Kind, ev.ObjectID = billing.EventPayment, o.ID
	ev.Metadata, ev.CustomerExternalID = o.Metadata, o.Customer.externalID()
	ev.PaymentSubscriptionID = str(o.SubscriptionID)
	ev.PaymentCharged = ev.Name == "order.paid" && o.chargesSubscription() && o.TotalAmount > 0 && ev.PaymentSubscriptionID != ""
	if p, ok := o.payment(c.testMode); ok {
		ev.Payments = []billing.Payment{p}
	}
	return nil
}

func (c *Client) refundEvent(ev *billing.Event, data json.RawMessage) error {
	r, err := decodeRefund(data)
	if err != nil {
		return err
	}
	ev.Kind, ev.ObjectID = billing.EventPayment, r.ID
	ev.Metadata, ev.PaymentSubscriptionID = r.Metadata, str(r.SubscriptionID)
	if p, ok := r.payment(c.testMode); ok {
		ev.Payments = []billing.Payment{p}
	}
	return nil
}
