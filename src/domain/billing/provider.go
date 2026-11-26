// Package billing is the provider-neutral billing core: the Provider contract
// a merchant-of-record adapter implements, the neutral subscription and event
// shapes, and the pure rules (liveness, plan ranking, the product map) the
// billing usecase builds on. Polar is the adapter (src/infra/billing/polar).
// Subscription statuses are Ogen's own words; the adapter maps the provider's
// into them, so the UI contract never sees a provider's vocabulary.
package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotConfigured is returned when the provider's access token or webhook
// secret is not set. Callers answer 503 billing_unavailable.
var ErrNotConfigured = errors.New("billing: provider not configured")

// ErrInvalidSignature is returned by VerifyWebhook for a body whose signature
// does not match, or whose timestamp is too far from now.
var ErrInvalidSignature = errors.New("billing: invalid webhook signature")

// ErrNotFound is returned when the provider has no such object.
var ErrNotFound = errors.New("billing: not found at provider")

// ErrPaymentFailed is returned when a change that charges now was declined or
// needs the customer to authenticate the payment.
var ErrPaymentFailed = errors.New("billing: payment failed at provider")

// ErrAlreadyCanceled is returned when a change targets a subscription the
// provider has already cancelled.
var ErrAlreadyCanceled = errors.New("billing: subscription already cancelled at provider")

// IsRetryable reports whether a provider error may succeed on retry. An error
// that knows (a Retryable() bool method, like an HTTP 429/5xx) decides;
// configuration, not-found, signature and refused-change errors never retry;
// anything else (a transport failure) does.
func IsRetryable(err error) bool {
	switch {
	case err == nil, errors.Is(err, ErrNotConfigured), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalidSignature),
		errors.Is(err, ErrPaymentFailed), errors.Is(err, ErrAlreadyCanceled):
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
	// CreateCheckout opens a hosted checkout for one product.
	CreateCheckout(ctx context.Context, req CheckoutRequest) (CheckoutSession, error)
	// GetSubscription reads one subscription.
	GetSubscription(ctx context.Context, providerSubID string) (Subscription, error)
	// ChangePlan switches a subscription to another product. Timing decides
	// whether it is charged now, applied from the next period, or applied now
	// without a charge.
	ChangePlan(ctx context.Context, providerSubID, productID string, opts ChangeOpts) (Subscription, error)
	// CancelScheduledChange drops a change the provider holds for the next
	// period, so the subscription renews on currentProductID, the product it
	// is on now.
	CancelScheduledChange(ctx context.Context, providerSubID, currentProductID string) (Subscription, error)
	// PortalSession opens a customer-portal session for the customer with
	// this external id (the tenant id).
	PortalSession(ctx context.Context, customerExternalID string) (PortalSession, error)
	// GetProduct reads a product's prices and trial in the current mode.
	GetProduct(ctx context.Context, productID string) (Product, error)
	// Catalog lists the organization's recurring products in one mode.
	// Reading the mode the server does not run in needs that mode's token;
	// ErrNotConfigured without it.
	Catalog(ctx context.Context, testMode bool) (Catalog, error)
	// VerifyWebhook checks a webhook body against its headers. It returns
	// ErrNotConfigured without a signing secret and ErrInvalidSignature on a
	// mismatch.
	VerifyWebhook(ctx context.Context, h WebhookHeaders, body []byte) error
	// ParseWebhook decodes a verified webhook body.
	ParseWebhook(h WebhookHeaders, body []byte) (Event, error)
	// PaymentSources lists the payment histories ListPayments reads, in
	// order.
	PaymentSources() []string
	// ListPayments reads one page (from 1) of one payment history of the
	// running mode.
	ListPayments(ctx context.Context, source string, page int) (PaymentPage, error)
}

// Remote plans the provider calls a billing job in river-service makes with
// its own leased token, and decodes their answers, by running the ordinary
// Provider over a transport that captures the request or replays the answer.
// The Polar client implements it.
type Remote interface {
	// Upstream names the API the call goes to, for river-service.
	Upstream() string
	// Plan returns the request op would send, without sending it.
	Plan(op func(p Provider) error) (Call, error)
	// Decode runs op as if the provider had answered its request with res.
	Decode(res CallResult, op func(p Provider) error) error
}

// Call is one planned request, its path relative to the API base URL.
// river-service adds the Authorization header.
type Call struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    []byte
}

// CallResult is river-service's answer to a Call: the response, or why there
// was none.
type CallResult struct {
	Status int
	Body   []byte
	Err    string
}

// WebhookHeaders are the Standard Webhooks headers of a delivery.
type WebhookHeaders struct {
	// ID is the delivery's event id: the same on every redelivery.
	ID        string
	Timestamp string
	Signature string
}

// CheckoutRequest is a hosted checkout for one product.
type CheckoutRequest struct {
	ProductID string
	// CustomerExternalID ties the provider customer to the workspace. It is
	// always the tenant id: the provider never lets it change once set.
	CustomerExternalID string
	Email              string
	// Metadata is copied onto the resulting subscription and its orders.
	Metadata   map[string]string
	SuccessURL string
	TestMode   bool
	// SkipTrial removes the product's free trial from this checkout.
	SkipTrial bool
}

// CheckoutSession is the hosted checkout to send the customer to.
type CheckoutSession struct {
	URL       string
	ExpiresAt time.Time
}

// PortalSession is a signed, short-lived customer-portal link.
type PortalSession struct {
	URL       string
	ExpiresAt time.Time
}

// ChangeTiming is when a plan change takes effect and how it is charged.
type ChangeTiming int

const (
	// ChargeNow applies the change at once and charges the prorated
	// difference immediately.
	ChargeNow ChangeTiming = iota
	// NextPeriod holds the change at the provider until the next renewal,
	// which then charges the new price.
	NextPeriod
	// NoCharge applies the change at once and settles any difference on the
	// next invoice.
	NoCharge
)

// ChangeOpts tunes a plan change.
type ChangeOpts struct {
	Timing ChangeTiming
	// EndTrialNow ends a running free trial, so the new plan is charged now.
	EndTrialNow bool
}

// Subscription is the neutral view of a provider subscription.
type Subscription struct {
	ID         string
	CustomerID string
	// CustomerExternalID is the provider customer's external id: the tenant.
	CustomerExternalID string
	ProductID          string
	// Currency is the currency the subscription is billed in.
	Currency string
	// Status is an Ogen status (models.SubscriptionStatus*), or "" when the
	// provider status has no Ogen meaning (a checkout still being paid, a
	// status this build does not know): the mirror then keeps the status it
	// had, and a subscription not mirrored yet is not mirrored.
	Status string
	// ProviderStatus is the provider's raw status, for support.
	ProviderStatus string
	TestMode       bool
	RenewsAt       *time.Time
	EndsAt         *time.Time
	TrialEndsAt    *time.Time
	// CancelAtPeriodEnd: the owner cancelled; live until EndsAt.
	CancelAtPeriodEnd bool
	// PendingProductID is the product a change held for the next period
	// moves to at PendingEffectiveAt; "" when nothing is pending.
	PendingProductID   string
	PendingEffectiveAt *time.Time
	CardBrand          string
	CardLastFour       string
	// Metadata is the checkout's metadata, copied by the provider.
	Metadata map[string]string
	// UpdatedAt is the provider's own last-change time: the staleness guard.
	UpdatedAt time.Time
}

// Event kinds a webhook can carry.
const (
	EventSubscription = "subscription"
	EventPayment      = "payment"
	EventOther        = "other"
)

// Event is a parsed webhook.
type Event struct {
	// ID is the delivery's event id: the dedupe key.
	ID       string
	Name     string
	Kind     string
	TestMode bool
	// ObjectID is the id of the object the event is about.
	ObjectID string
	// Metadata is the checkout's metadata, echoed back.
	Metadata map[string]string
	// CustomerExternalID is the customer's external id, when the payload
	// carries the customer.
	CustomerExternalID string
	// Subscription is set for subscription snapshot events.
	Subscription *Subscription
	// PaymentSubscriptionID is the subscription a payment event belongs to.
	PaymentSubscriptionID string
	// PaymentCharged: the event is a successful non-zero charge of the
	// subscription, which separates a paying workspace from a trial that
	// never converted.
	PaymentCharged bool
	// Payments are the money movements the event reports.
	Payments []Payment
}

// ReconcilePlan is the provider work one reconcile pass needs: the
// subscriptions to re-read.
type ReconcilePlan struct {
	Subscriptions []string
}
