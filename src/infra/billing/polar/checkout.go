package polar

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// CreateCheckout implements billing.Provider. The customer is addressed by
// its external id (the tenant), so a second checkout of the workspace reuses
// the Polar customer. Discount codes are never offered.
func (c *Client) CreateCheckout(ctx context.Context, in billing.CheckoutRequest) (billing.CheckoutSession, error) {
	body := map[string]any{
		"products":             []string{in.ProductID},
		"external_customer_id": in.CustomerExternalID,
		"success_url":          in.SuccessURL,
		"allow_trial":          !in.SkipTrial,
		"allow_discount_codes": false,
	}
	if in.Email != "" {
		body["customer_email"] = in.Email
	}
	if len(in.Metadata) > 0 {
		body["metadata"] = in.Metadata
	}
	var out struct {
		URL       string     `json:"url"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodPost, path: "/checkouts/", body: body}, &out); err != nil {
		return billing.CheckoutSession{}, err
	}
	if out.URL == "" {
		return billing.CheckoutSession{}, errors.New("polar: checkout has no url")
	}
	session := billing.CheckoutSession{URL: out.URL}
	if out.ExpiresAt != nil {
		session.ExpiresAt = out.ExpiresAt.UTC()
	}
	return session, nil
}

// PortalSession implements billing.Provider: a fresh customer session each
// time. The link is short-lived and must not be logged or cached.
func (c *Client) PortalSession(ctx context.Context, customerExternalID string) (billing.PortalSession, error) {
	var out struct {
		URL       string     `json:"customer_portal_url"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	body := map[string]any{"external_customer_id": customerExternalID}
	if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodPost, path: "/customer-sessions/", body: body}, &out); err != nil {
		return billing.PortalSession{}, err
	}
	if out.URL == "" {
		return billing.PortalSession{}, errors.New("polar: customer session has no portal url")
	}
	session := billing.PortalSession{URL: out.URL}
	if out.ExpiresAt != nil {
		session.ExpiresAt = out.ExpiresAt.UTC()
	}
	return session, nil
}
