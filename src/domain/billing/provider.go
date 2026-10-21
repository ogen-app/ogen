// Package billing is the provider-neutral billing core: the Provider contract
// a merchant-of-record adapter implements, the neutral subscription and event
// shapes, and the pure rules (liveness, plan ranking, the variant map) the
// billing usecase builds on. Lemon Squeezy is the only adapter today
// (src/infra/billing/lemonsqueezy); subscription statuses keep its vocabulary
// because the UI contract passes them through.
package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotConfigured is returned when the provider's API key or webhook secret
// is not set. Callers answer 503 billing_unavailable.
var ErrNotConfigured = errors.New("billing: provider not configured")

// ErrInvalidSignature is returned by VerifyWebhook for a body whose signature
// does not match.
var ErrInvalidSignature = errors.New("billing: invalid webhook signature")

// ErrNotFound is returned when the provider has no such object.
var ErrNotFound = errors.New("billing: not found at provider")

// Provider is a merchant-of-record adapter.
type Provider interface {
	// CreateCheckout opens a hosted checkout for one variant.
	CreateCheckout(ctx context.Context, req CheckoutRequest) (CheckoutSession, error)
	// GetSubscription reads one subscription, including a fresh portal URL.
	GetSubscription(ctx context.Context, providerSubID string) (Subscription, error)
	// ChangePlan switches a subscription to another variant. It applies at
	// once at the provider; scheduling is the caller's job.
	ChangePlan(ctx context.Context, providerSubID, variantID string, opts ChangeOpts) (Subscription, error)
	// GetVariant reads a variant's price and trial configuration.
	GetVariant(ctx context.Context, variantID string) (Variant, error)
	// VerifyWebhook checks a webhook body against its signature header. It
	// returns ErrNotConfigured without a signing secret and
	// ErrInvalidSignature on a mismatch.
	VerifyWebhook(ctx context.Context, signature string, body []byte) error
	// ParseWebhook decodes a verified webhook body.
	ParseWebhook(body []byte) (Event, error)
}

// CheckoutRequest is a hosted checkout for one variant.
type CheckoutRequest struct {
	VariantID string
	Email     string
	// Custom is echoed back on every webhook of the resulting subscription.
	Custom      map[string]string
	RedirectURL string
	ExpiresAt   time.Time
	TestMode    bool
	// SkipTrial removes the variant's free trial from this checkout.
	SkipTrial bool
}

// CheckoutSession is the hosted checkout to send the customer to.
type CheckoutSession struct {
	URL       string
	ExpiresAt time.Time
}

// ChangeOpts tunes a plan change.
type ChangeOpts struct {
	// InvoiceNow charges the prorated difference immediately.
	InvoiceNow bool
	// NoProration applies the new price from the next renewal, with no
	// proration charge. It overrides InvoiceNow at the provider.
	NoProration bool
	// EndTrialNow ends a running free trial, so the new plan is charged now.
	EndTrialNow bool
}

// Subscription is the neutral view of a provider subscription.
type Subscription struct {
	ID           string
	CustomerID   string
	OrderID      string
	VariantID    string
	Status       string
	TestMode     bool
	RenewsAt     *time.Time
	EndsAt       *time.Time
	TrialEndsAt  *time.Time
	CardBrand    string
	CardLastFour string
	// UpdatedAt is the provider's own last-change time: the staleness guard.
	UpdatedAt time.Time
	// PortalURL is a signed customer-portal link; it expires (24h at Lemon
	// Squeezy) and is only present on a direct read.
	PortalURL string
}

// Variant is a purchasable provider variant.
type Variant struct {
	ID         string
	PriceMinor int64
	// Interval is month | year for a subscription variant.
	Interval string
	// HasFreeTrial and TrialInterval/TrialIntervalCount describe the trial
	// the variant carries (e.g. month / 1).
	HasFreeTrial       bool
	TrialInterval      string
	TrialIntervalCount int
	TestMode           bool
}

// OneMonthTrial reports whether the variant carries exactly a one-month free
// trial: the free-month offer's precondition.
func (v Variant) OneMonthTrial() bool {
	return v.HasFreeTrial && v.TrialInterval == "month" && v.TrialIntervalCount == 1
}

// TrialDays approximates the trial length in days for display.
func (v Variant) TrialDays() int {
	if !v.HasFreeTrial {
		return 0
	}
	per := map[string]int{"day": 1, "week": 7, "month": 30, "year": 365}[v.TrialInterval]
	return per * v.TrialIntervalCount
}

// Event kinds a webhook can carry.
const (
	EventSubscription = "subscription"
	EventPayment      = "payment"
	EventOther        = "other"
)

// Event is a parsed webhook.
type Event struct {
	Name     string
	Kind     string
	TestMode bool
	// ObjectID is the id of the object the event is about.
	ObjectID string
	// CustomData is the checkout's custom data, echoed back.
	CustomData map[string]string
	// Subscription is set for subscription_* snapshot events.
	Subscription *Subscription
	// Payment fields are set for subscription_payment_* events.
	PaymentSubscriptionID string
	PaymentAmountMinor    int64
	PaymentStatus         string
}
