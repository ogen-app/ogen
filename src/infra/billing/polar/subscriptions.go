package polar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// customerJSON is the customer object embedded in subscriptions and orders.
type customerJSON struct {
	ID         string  `json:"id"`
	ExternalID *string `json:"external_id"`
}

func (c *customerJSON) externalID() string {
	if c == nil || c.ExternalID == nil {
		return ""
	}
	return *c.ExternalID
}

// subscriptionJSON is the subset of the subscription object we mirror.
type subscriptionJSON struct {
	ID                string        `json:"id"`
	CreatedAt         time.Time     `json:"created_at"`
	ModifiedAt        *time.Time    `json:"modified_at"`
	Status            string        `json:"status"`
	Currency          string        `json:"currency"`
	CurrentPeriodEnd  *time.Time    `json:"current_period_end"`
	TrialEnd          *time.Time    `json:"trial_end"`
	CancelAtPeriodEnd bool          `json:"cancel_at_period_end"`
	EndsAt            *time.Time    `json:"ends_at"`
	EndedAt           *time.Time    `json:"ended_at"`
	CustomerID        string        `json:"customer_id"`
	ProductID         string        `json:"product_id"`
	Customer          *customerJSON `json:"customer"`
	Metadata          metadata      `json:"metadata"`
	// PendingUpdate is the change held for the next period (spike: field
	// names).
	PendingUpdate *struct {
		ProductID *string    `json:"product_id"`
		AppliesAt *time.Time `json:"applies_at"`
	} `json:"pending_update"`
}

// Polar's subscription statuses.
const (
	statusIncomplete        = "incomplete"
	statusIncompleteExpired = "incomplete_expired"
	statusTrialing          = "trialing"
	statusActive            = "active"
	statusPastDue           = "past_due"
	statusCanceled          = "canceled"
	statusUnpaid            = "unpaid"
	statusPaused            = "paused"
	statusTrialEnded        = "trial_ended"
)

// MapStatus maps a Polar subscription status into Ogen's words: the only
// place Polar's vocabulary is read. A cancellation at period end keeps Polar
// active or trialing until the end; Ogen calls it cancelled, live until
// ends_at. A canceled subscription is cancelled while its end is ahead and
// expired once it has passed; a trial that ended unpaid is expired. "" means
// the status has no Ogen meaning: a checkout that was never paid, or a status
// this build does not know.
func MapStatus(status string, cancelAtPeriodEnd bool, endedAt *time.Time, now time.Time) string {
	switch status {
	case statusTrialing:
		if cancelAtPeriodEnd {
			return models.SubscriptionStatusCancelled
		}
		return models.SubscriptionStatusOnTrial
	case statusActive:
		if cancelAtPeriodEnd {
			return models.SubscriptionStatusCancelled
		}
		return models.SubscriptionStatusActive
	case statusPastDue:
		return models.SubscriptionStatusPastDue
	case statusUnpaid:
		return models.SubscriptionStatusUnpaid
	case statusPaused:
		return models.SubscriptionStatusPaused
	case statusCanceled:
		if endedAt != nil && endedAt.After(now) {
			return models.SubscriptionStatusCancelled
		}
		return models.SubscriptionStatusExpired
	case statusTrialEnded:
		return models.SubscriptionStatusExpired
	default:
		return ""
	}
}

func (s subscriptionJSON) toSubscription(testMode bool, now time.Time) billing.Subscription {
	out := billing.Subscription{
		ID: s.ID, CustomerID: s.CustomerID, CustomerExternalID: s.Customer.externalID(),
		ProductID: s.ProductID, Currency: s.Currency, ProviderStatus: s.Status, TestMode: testMode,
		TrialEndsAt: s.TrialEnd, CancelAtPeriodEnd: s.CancelAtPeriodEnd, Metadata: s.Metadata,
		UpdatedAt: s.CreatedAt,
	}
	if s.ModifiedAt != nil {
		out.UpdatedAt = *s.ModifiedAt
	}
	ended := s.EndedAt
	if ended == nil {
		ended = s.EndsAt
	}
	out.Status = MapStatus(s.Status, s.CancelAtPeriodEnd, ended, now)
	switch out.Status {
	case models.SubscriptionStatusOnTrial, models.SubscriptionStatusActive, models.SubscriptionStatusPastDue, models.SubscriptionStatusUnpaid:
		out.RenewsAt = s.CurrentPeriodEnd
	case models.SubscriptionStatusCancelled:
		out.EndsAt = cancelledEnd(s)
	case models.SubscriptionStatusExpired:
		out.EndsAt = ended
	case "":
		if s.Status != statusIncomplete && s.Status != statusIncompleteExpired {
			slog.Warn("polar: unknown subscription status; the mirror keeps its status", logging.AttrComponent, "billing.polar",
				"subscription_id", s.ID, "status", s.Status)
		}
	}
	if p := s.PendingUpdate; p != nil && p.ProductID != nil && *p.ProductID != s.ProductID {
		out.PendingProductID = *p.ProductID
		out.PendingEffectiveAt = p.AppliesAt
		if out.PendingEffectiveAt == nil {
			out.PendingEffectiveAt = s.CurrentPeriodEnd
		}
	}
	return out
}

// cancelledEnd is when a cancelled subscription stops entitling: the end
// Polar names, else the trial's or the period's end.
func cancelledEnd(s subscriptionJSON) *time.Time {
	switch {
	case s.EndsAt != nil:
		return s.EndsAt
	case s.EndedAt != nil:
		return s.EndedAt
	case s.Status == statusTrialing && s.TrialEnd != nil:
		return s.TrialEnd
	}
	return s.CurrentPeriodEnd
}

func (c *Client) decodeSubscription(raw json.RawMessage) (billing.Subscription, error) {
	var s subscriptionJSON
	if err := json.Unmarshal(raw, &s); err != nil {
		return billing.Subscription{}, fmt.Errorf("polar: decode subscription: %w", err)
	}
	if s.ID == "" {
		return billing.Subscription{}, errNoID
	}
	return s.toSubscription(c.testMode, c.now().UTC()), nil
}

// GetSubscription implements billing.Provider.
func (c *Client) GetSubscription(ctx context.Context, id string) (billing.Subscription, error) {
	var raw json.RawMessage
	if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodGet, path: "/subscriptions/" + url.PathEscape(id)}, &raw); err != nil {
		return billing.Subscription{}, err
	}
	return c.decodeSubscription(raw)
}

// prorationBehavior is Polar's name for a change's timing.
func prorationBehavior(t billing.ChangeTiming) string {
	switch t {
	case billing.NextPeriod:
		return "next_period"
	case billing.NoCharge:
		return "prorate"
	default:
		return "invoice"
	}
}

// ChangePlan implements billing.Provider. Ending the trial in the same
// update charges the new product at once (spike: one PATCH).
func (c *Client) ChangePlan(ctx context.Context, id, productID string, opts billing.ChangeOpts) (billing.Subscription, error) {
	body := map[string]any{"product_id": productID, "proration_behavior": prorationBehavior(opts.Timing)}
	if opts.EndTrialNow {
		body["trial_end"] = "now"
	}
	return c.patchSubscription(ctx, id, body)
}

// CancelScheduledChange implements billing.Provider with Polar's clearing
// update ({"pending_update": null}), which needs nothing of the current
// product: it works even when that product was archived since.
func (c *Client) CancelScheduledChange(ctx context.Context, id string) (billing.Subscription, error) {
	return c.patchSubscription(ctx, id, map[string]any{"pending_update": nil})
}

func (c *Client) patchSubscription(ctx context.Context, id string, body map[string]any) (billing.Subscription, error) {
	var raw json.RawMessage
	if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodPatch, path: "/subscriptions/" + url.PathEscape(id), body: body}, &raw); err != nil {
		return billing.Subscription{}, err
	}
	return c.decodeSubscription(raw)
}

// metadata reads Polar metadata, whose values are strings, numbers or
// booleans, as strings.
type metadata map[string]string

func (m *metadata) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
			continue
		}
		var n json.Number
		if json.Unmarshal(v, &n) == nil {
			out[k] = n.String()
			continue
		}
		var bv bool
		if json.Unmarshal(v, &bv) == nil {
			out[k] = strconv.FormatBool(bv)
		}
	}
	*m = out
	return nil
}
