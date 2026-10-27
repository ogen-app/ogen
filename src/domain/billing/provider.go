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

// IsRetryable reports whether a provider error may succeed on retry. An error
// that knows (a Retryable() bool method, like an HTTP 429/5xx) decides;
// configuration, not-found and signature errors never retry; anything else
// (a transport failure) does.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidSignature) {
		return false
	}
	if r, ok := errors.AsType[interface {
		error
		Retryable() bool
	}](err); ok {
		return r.Retryable()
	}
	return true
}

// Provider is a merchant-of-record adapter.
type Provider interface {
	// CreateCheckout opens a hosted checkout for one variant.
	CreateCheckout(ctx context.Context, req CheckoutRequest) (CheckoutSession, error)
	// GetSubscription reads one subscription, including a fresh portal URL.
	GetSubscription(ctx context.Context, providerSubID string) (Subscription, error)
	// ChangePlan switches a subscription to another variant. It applies at
	// once at the provider; scheduling is the caller's job.
	ChangePlan(ctx context.Context, providerSubID, variantID string, opts ChangeOpts) (Subscription, error)
	// GetVariant reads a variant's price and trial configuration in the
	// current mode.
	GetVariant(ctx context.Context, variantID string) (Variant, error)
	// Catalog lists the store's subscription variants in one mode, with the
	// store's currency. Reading the mode the server does not run in needs
	// that mode's API key; ErrNotConfigured without it.
	Catalog(ctx context.Context, testMode bool) (Catalog, error)
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
	// Interval is month | year for a subscription variant; IntervalCount is
	// how many of them one billing period spans (0 reads as 1).
	Interval      string
	IntervalCount int
	// HasFreeTrial and TrialInterval/TrialIntervalCount describe the trial
	// the variant carries (e.g. month / 1).
	HasFreeTrial       bool
	TrialInterval      string
	TrialIntervalCount int
	TestMode           bool
	// Catalog details, filled by Provider.Catalog for the mapping picker.
	ProductID   string
	ProductName string
	Name        string
	// Status is the provider's variant status, e.g. published.
	Status string
}

// Catalog is a store's subscription variants in one mode.
type Catalog struct {
	TestMode bool
	// Currency is the store's currency: every variant is priced in it.
	Currency string
	Variants []Variant
}

// Variant finds a variant by id.
func (c Catalog) Variant(id string) (Variant, bool) {
	for _, v := range c.Variants {
		if v.ID == id {
			return v, true
		}
	}
	return Variant{}, false
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

// PlanSwitch is a provider plan switch a scheduled change needs. A job
// running in river-service has it make the call.
type PlanSwitch struct {
	SubscriptionID string `json:"subscription_id"`
	VariantID      string `json:"variant_id"`
}

// Opts are the switch's options: never prorated, so the renewal charges the
// new price.
func (PlanSwitch) Opts() ChangeOpts { return ChangeOpts{NoProration: true} }

// ReconcilePlan is the provider work one reconcile pass needs: the
// subscriptions to re-read and the due scheduled changes to re-drive.
type ReconcilePlan struct {
	Subscriptions []string
	DueChanges    []string
}
