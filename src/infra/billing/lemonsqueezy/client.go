// Package lemonsqueezy is the Lemon Squeezy adapter behind billing.Provider.
//
// The API is JSON:API. Decoding is tolerant: ids arrive as strings in data.id
// and as numbers in attributes, so both are read through flexString, and
// unknown attributes are ignored. The API key and webhook secret are resolved
// per call from the secret store, so a Harbor rotation applies without a
// restart; neither is ever logged.
package lemonsqueezy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

const (
	defaultBaseURL = "https://api.lemonsqueezy.com/v1"
	defaultTimeout = 15 * time.Second
	mediaType      = "application/vnd.api+json"
	errorBodyLimit = 4 << 10
)

// SecretResolver returns a secret's current value; "" means not set.
type SecretResolver func(ctx context.Context) (string, error)

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("lemonsqueezy: HTTP %d", e.Status)
	}
	return fmt.Sprintf("lemonsqueezy: HTTP %d: %s", e.Status, e.Detail)
}

// Retryable reports whether a retry may succeed: throttling and server errors.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// IsRetryable reports whether err is worth retrying: a retryable APIError or a
// transport failure. Configuration and other 4xx errors are terminal.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, billing.ErrNotConfigured) || errors.Is(err, billing.ErrNotFound) {
		return false
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Retryable()
	}
	return true
}

// Client implements billing.Provider for Lemon Squeezy.
type Client struct {
	http          *http.Client
	baseURL       string
	storeID       string
	apiKey        SecretResolver
	webhookSecret SecretResolver
}

var _ billing.Provider = (*Client)(nil)

// New builds a client. baseURL and timeout fall back to the defaults.
func New(apiKey, webhookSecret SecretResolver, storeID, baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		http:          &http.Client{Timeout: timeout},
		baseURL:       strings.TrimRight(cmp.Or(baseURL, defaultBaseURL), "/"),
		storeID:       storeID,
		apiKey:        apiKey,
		webhookSecret: webhookSecret,
	}
}

// String never includes credentials.
func (c *Client) String() string { return fmt.Sprintf("lemonsqueezy.Client(baseURL=%s)", c.baseURL) }

// resource is one JSON:API resource object.
type resource struct {
	Type       string          `json:"type"`
	ID         flexString      `json:"id"`
	Attributes json.RawMessage `json:"attributes"`
}

type document struct {
	Data resource `json:"data"`
}

// flexString reads a JSON string or number as a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("lemonsqueezy: id %s is neither string nor number", b)
	}
	*f = flexString(n.String())
	return nil
}

// flexTime reads an RFC 3339 timestamp, tolerating null and "".
type flexTime struct{ t *time.Time }

func (f *flexTime) UnmarshalJSON(b []byte) error {
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == nil || *s == "" {
		f.t = nil
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return fmt.Errorf("lemonsqueezy: bad timestamp %q: %w", *s, err)
	}
	t = t.UTC()
	f.t = &t
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	key, err := c.apiKey(ctx)
	if err != nil {
		return fmt.Errorf("lemonsqueezy: resolve api key: %w", err)
	}
	if key == "" {
		return billing.ErrNotConfigured
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("lemonsqueezy: marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", mediaType)
	if body != nil {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL holds no secret; the bearer token is only in a header.
		return fmt.Errorf("lemonsqueezy: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s %s", billing.ErrNotFound, method, path)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return &APIError{Status: resp.StatusCode, Detail: errorDetail(raw)}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("lemonsqueezy: decode response: %w", err)
		}
	}
	return nil
}

// errorDetail extracts the first JSON:API error detail, falling back to a
// truncated body.
func errorDetail(raw []byte) string {
	var env struct {
		Errors []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Errors) > 0 {
		return cmp.Or(env.Errors[0].Detail, env.Errors[0].Title)
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}

func numericID(id string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("lemonsqueezy: id %q is not numeric", id)
	}
	return n, nil
}

type relationship struct {
	Data struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"data"`
}

func rel(typ, id string) relationship {
	var r relationship
	r.Data.Type, r.Data.ID = typ, id
	return r
}

// CreateCheckout implements billing.Provider.
func (c *Client) CreateCheckout(ctx context.Context, in billing.CheckoutRequest) (billing.CheckoutSession, error) {
	attrs := map[string]any{
		"checkout_data":    map[string]any{"email": in.Email, "custom": in.Custom},
		"product_options":  map[string]any{"redirect_url": in.RedirectURL},
		"checkout_options": map[string]any{"skip_trial": in.SkipTrial},
		"test_mode":        in.TestMode,
	}
	if !in.ExpiresAt.IsZero() {
		attrs["expires_at"] = in.ExpiresAt.UTC().Format(time.RFC3339)
	}
	body := map[string]any{"data": map[string]any{
		"type":       "checkouts",
		"attributes": attrs,
		"relationships": map[string]any{
			"store":   rel("stores", c.storeID),
			"variant": rel("variants", in.VariantID),
		},
	}}
	var doc document
	if err := c.do(ctx, http.MethodPost, "/checkouts", body, &doc); err != nil {
		return billing.CheckoutSession{}, err
	}
	var a struct {
		URL       string   `json:"url"`
		ExpiresAt flexTime `json:"expires_at"`
	}
	if err := json.Unmarshal(doc.Data.Attributes, &a); err != nil {
		return billing.CheckoutSession{}, fmt.Errorf("lemonsqueezy: decode checkout: %w", err)
	}
	if a.URL == "" {
		return billing.CheckoutSession{}, errors.New("lemonsqueezy: checkout has no url")
	}
	out := billing.CheckoutSession{URL: a.URL, ExpiresAt: in.ExpiresAt}
	if a.ExpiresAt.t != nil {
		out.ExpiresAt = *a.ExpiresAt.t
	}
	return out, nil
}

// subscriptionAttrs is the subset of the subscription object we mirror.
type subscriptionAttrs struct {
	CustomerID   flexString `json:"customer_id"`
	OrderID      flexString `json:"order_id"`
	VariantID    flexString `json:"variant_id"`
	Status       string     `json:"status"`
	CardBrand    string     `json:"card_brand"`
	CardLastFour string     `json:"card_last_four"`
	TrialEndsAt  flexTime   `json:"trial_ends_at"`
	RenewsAt     flexTime   `json:"renews_at"`
	EndsAt       flexTime   `json:"ends_at"`
	UpdatedAt    flexTime   `json:"updated_at"`
	TestMode     bool       `json:"test_mode"`
	URLs         struct {
		CustomerPortal string `json:"customer_portal"`
	} `json:"urls"`
}

func (a subscriptionAttrs) toSubscription(id string) billing.Subscription {
	s := billing.Subscription{
		ID:           id,
		CustomerID:   string(a.CustomerID),
		OrderID:      string(a.OrderID),
		VariantID:    string(a.VariantID),
		Status:       a.Status,
		TestMode:     a.TestMode,
		RenewsAt:     a.RenewsAt.t,
		EndsAt:       a.EndsAt.t,
		TrialEndsAt:  a.TrialEndsAt.t,
		CardBrand:    a.CardBrand,
		CardLastFour: a.CardLastFour,
		PortalURL:    a.URLs.CustomerPortal,
	}
	if a.UpdatedAt.t != nil {
		s.UpdatedAt = *a.UpdatedAt.t
	}
	return s
}

func decodeSubscription(doc document) (billing.Subscription, error) {
	var a subscriptionAttrs
	if err := json.Unmarshal(doc.Data.Attributes, &a); err != nil {
		return billing.Subscription{}, fmt.Errorf("lemonsqueezy: decode subscription: %w", err)
	}
	return a.toSubscription(string(doc.Data.ID)), nil
}

// GetSubscription implements billing.Provider.
func (c *Client) GetSubscription(ctx context.Context, id string) (billing.Subscription, error) {
	var doc document
	if err := c.do(ctx, http.MethodGet, "/subscriptions/"+id, nil, &doc); err != nil {
		return billing.Subscription{}, err
	}
	return decodeSubscription(doc)
}

// ChangePlan implements billing.Provider.
func (c *Client) ChangePlan(ctx context.Context, id, variantID string, opts billing.ChangeOpts) (billing.Subscription, error) {
	vid, err := numericID(variantID)
	if err != nil {
		return billing.Subscription{}, err
	}
	attrs := map[string]any{"variant_id": vid}
	if opts.NoProration {
		attrs["disable_prorations"] = true
	} else if opts.InvoiceNow {
		attrs["invoice_immediately"] = true
	}
	if opts.EndTrialNow {
		attrs["trial_ends_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	body := map[string]any{"data": map[string]any{"type": "subscriptions", "id": id, "attributes": attrs}}
	var doc document
	if err := c.do(ctx, http.MethodPatch, "/subscriptions/"+id, body, &doc); err != nil {
		return billing.Subscription{}, err
	}
	return decodeSubscription(doc)
}

// GetVariant implements billing.Provider.
func (c *Client) GetVariant(ctx context.Context, id string) (billing.Variant, error) {
	var doc document
	if err := c.do(ctx, http.MethodGet, "/variants/"+id, nil, &doc); err != nil {
		return billing.Variant{}, err
	}
	var a struct {
		Price              int64   `json:"price"`
		Interval           *string `json:"interval"`
		HasFreeTrial       bool    `json:"has_free_trial"`
		TrialInterval      *string `json:"trial_interval"`
		TrialIntervalCount *int    `json:"trial_interval_count"`
		TestMode           bool    `json:"test_mode"`
	}
	if err := json.Unmarshal(doc.Data.Attributes, &a); err != nil {
		return billing.Variant{}, fmt.Errorf("lemonsqueezy: decode variant: %w", err)
	}
	v := billing.Variant{ID: string(doc.Data.ID), PriceMinor: a.Price, HasFreeTrial: a.HasFreeTrial, TestMode: a.TestMode}
	if a.Interval != nil {
		v.Interval = *a.Interval
	}
	if a.TrialInterval != nil {
		v.TrialInterval = *a.TrialInterval
	}
	if a.TrialIntervalCount != nil {
		v.TrialIntervalCount = *a.TrialIntervalCount
	}
	return v, nil
}
