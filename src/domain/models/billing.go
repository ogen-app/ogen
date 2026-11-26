package models

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// Billing providers. Polar is the one billing runs on; Lemon Squeezy names the
// history rows of the provider before it.
const (
	BillingProviderPolar        = "polar"
	BillingProviderLemonSqueezy = "lemonsqueezy"
)

// Subscription statuses: Ogen's own words, whatever the provider. The
// provider adapter maps its statuses into them; the raw one is kept in
// provider_status.
const (
	SubscriptionStatusOnTrial   = "on_trial"
	SubscriptionStatusActive    = "active"
	SubscriptionStatusPaused    = "paused"
	SubscriptionStatusPastDue   = "past_due"
	SubscriptionStatusUnpaid    = "unpaid"
	SubscriptionStatusCancelled = "cancelled"
	SubscriptionStatusExpired   = "expired"
)

// Why a subscription was invalidated.
const (
	// InvalidatedReasonModeCutover marks a test-mode subscription invalidated
	// at the sandbox-to-live cutover.
	InvalidatedReasonModeCutover = "mode_cutover"
	// InvalidatedReasonProviderSwitch marks a subscription of the previous
	// provider, invalidated when billing moved to the current one.
	InvalidatedReasonProviderSwitch = "provider_switch"
)

// BillingSubscription mirrors one provider subscription of a workspace.
type BillingSubscription struct {
	bun.BaseModel `bun:"table:billing_subscriptions,alias:bs" swaggerignore:"true"`

	ID                     string  `bun:"id,pk"`
	TenantID               string  `bun:"tenant_id,notnull"`
	AccountID              *string `bun:"account_id"`
	Provider               string  `bun:"provider,notnull"`
	ProviderSubscriptionID string  `bun:"provider_subscription_id,notnull"`
	ProviderCustomerID     string  `bun:"provider_customer_id,notnull"`
	ProviderOrderID        string  `bun:"provider_order_id,notnull"`
	// ProviderVariantID is written alongside ProviderProductID while the
	// previous release, which reads it, can still be rolled back to.
	ProviderVariantID  string     `bun:"provider_variant_id,notnull"`
	ProviderProductID  string     `bun:"provider_product_id,notnull"`
	TierVersionPriceID *string    `bun:"tier_version_price_id"`
	Status             string     `bun:"status,notnull"`
	ProviderStatus     string     `bun:"provider_status,notnull"`
	CancelAtPeriodEnd  bool       `bun:"cancel_at_period_end,notnull"`
	PendingProductID   string     `bun:"pending_product_id,notnull"`
	PendingEffectiveAt *time.Time `bun:"pending_effective_at"`
	TestMode           bool       `bun:"test_mode,notnull"`
	RenewsAt           *time.Time `bun:"renews_at"`
	EndsAt             *time.Time `bun:"ends_at"`
	TrialEndsAt        *time.Time `bun:"trial_ends_at"`
	FirstPaidAt        *time.Time `bun:"first_paid_at"`
	CardBrand          string     `bun:"card_brand,notnull"`
	CardLastFour       string     `bun:"card_last_four,notnull"`
	ProviderUpdatedAt  time.Time  `bun:"provider_updated_at,notnull"`
	InvalidatedAt      *time.Time `bun:"invalidated_at"`
	InvalidatedReason  string     `bun:"invalidated_reason,notnull"`
	TeardownAt         *time.Time `bun:"teardown_at"`
	CreatedAt          time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt          time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
}

// Webhook ingest outcomes.
const (
	WebhookOutcomeApplied        = "applied"
	WebhookOutcomeStale          = "stale"
	WebhookOutcomeIgnoredMode    = "ignored_mode"
	WebhookOutcomeIgnoredUnknown = "ignored_unknown"
	WebhookOutcomeFailed         = "failed"
)

// BillingWebhookEvent is one provider webhook delivery. ProviderEventID is
// the dedupe key; BodySHA256 deduped Lemon Squeezy, which sent no event id.
type BillingWebhookEvent struct {
	bun.BaseModel `bun:"table:billing_webhook_events,alias:bwe" swaggerignore:"true"`

	ID               string          `bun:"id,pk"`
	Provider         string          `bun:"provider,notnull"`
	EventName        string          `bun:"event_name,notnull"`
	BodySHA256       string          `bun:"body_sha256,notnull"`
	ProviderEventID  *string         `bun:"provider_event_id"`
	ProviderObjectID string          `bun:"provider_object_id,notnull"`
	TenantID         *string         `bun:"tenant_id"`
	TestMode         bool            `bun:"test_mode,notnull"`
	ReceivedAt       time.Time       `bun:"received_at,notnull,default:current_timestamp"`
	ProcessedAt      *time.Time      `bun:"processed_at"`
	Outcome          string          `bun:"outcome,notnull"`
	Error            string          `bun:"error,notnull"`
	Payload          json.RawMessage `bun:"payload,type:jsonb,notnull"`
}

// Scheduled change statuses.
const (
	ScheduledChangePending         = "pending"
	ScheduledChangeProviderApplied = "provider_applied"
	ScheduledChangeApplied         = "applied"
	ScheduledChangeCancelled       = "cancelled"
	ScheduledChangeFailed          = "failed"
)

// BillingScheduledChange is a downgrade landing at the billing boundary. The
// provider holds it from the moment it is made (provider_applied) and applies
// it at renewal; the renewal's snapshot marks it applied. pending is the
// Lemon Squeezy flow, whose provider switch ran an hour before renewal.
type BillingScheduledChange struct {
	bun.BaseModel `bun:"table:billing_scheduled_changes,alias:bsc" swaggerignore:"true"`

	ID                     string    `bun:"id,pk"`
	TenantID               string    `bun:"tenant_id,notnull"`
	BillingSubscriptionID  string    `bun:"billing_subscription_id,notnull"`
	FromTierVersionPriceID string    `bun:"from_tier_version_price_id,notnull"`
	ToTierVersionPriceID   string    `bun:"to_tier_version_price_id,notnull"`
	EffectiveAt            time.Time `bun:"effective_at,notnull"`
	Status                 string    `bun:"status,notnull"`
	CreatedBy              string    `bun:"created_by,notnull"`
	Error                  string    `bun:"error,notnull"`
	CreatedAt              time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt              time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

// Open reports whether the change still has work to do.
func (c *BillingScheduledChange) Open() bool {
	return c.Status == ScheduledChangePending || c.Status == ScheduledChangeProviderApplied
}

// ProductMappingByHarbor marks a product mapping set from Harbor.
const ProductMappingByHarbor = "harbor"

// BillingProductMapping ties a tier version price row to the provider product
// that sells it in the row's currency, separately per mode. An operator-wide
// row, not tenant scoped.
type BillingProductMapping struct {
	bun.BaseModel `bun:"table:billing_product_mappings,alias:bpm" swaggerignore:"true"`

	TierVersionPriceID string    `bun:"tier_version_price_id,pk"                     json:"tier_version_price_id"`
	TestMode           bool      `bun:"test_mode,pk"                                 json:"test_mode"`
	ProductID          string    `bun:"product_id,notnull"                           json:"product_id"`
	Currency           string    `bun:"currency,notnull"                             json:"currency"`
	UpdatedAt          time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	UpdatedBy          string    `bun:"updated_by,notnull"                           json:"updated_by"`
	Reason             string    `bun:"reason,notnull"                               json:"reason"`
}

// BillingPayment is one provider money movement: an order, a refund, or for
// Lemon Squeezy history a subscription invoice. Amounts are in minor units,
// with *USD in US cents; the USD amounts are nil while a non-USD payment waits
// for its exchange rate. TenantID comes from the payload when it carried one.
type BillingPayment struct {
	bun.BaseModel `bun:"table:billing_payments,alias:bp" swaggerignore:"true"`

	ID                     string  `bun:"id,pk"`
	Provider               string  `bun:"provider,notnull"`
	Source                 string  `bun:"source,notnull"`
	ProviderID             string  `bun:"provider_id,notnull"`
	Kind                   string  `bun:"kind,notnull"`
	TestMode               bool    `bun:"test_mode,notnull"`
	TenantID               *string `bun:"tenant_id"`
	ProviderSubscriptionID string  `bun:"provider_subscription_id,notnull"`
	// ProviderVariantID mirrors ProviderProductID for the rollback window.
	ProviderVariantID string `bun:"provider_variant_id,notnull"`
	ProviderProductID string `bun:"provider_product_id,notnull"`
	BillingReason     string `bun:"billing_reason,notnull"`
	Currency          string `bun:"currency,notnull"`
	Subtotal          int64  `bun:"subtotal,notnull"`
	Discount          int64  `bun:"discount,notnull"`
	Tax               int64  `bun:"tax,notnull"`
	Total             int64  `bun:"total,notnull"`
	Refunded          int64  `bun:"refunded,notnull"`
	SubtotalUSD       *int64 `bun:"subtotal_usd"`
	DiscountUSD       *int64 `bun:"discount_usd"`
	TaxUSD            *int64 `bun:"tax_usd"`
	TotalUSD          *int64 `bun:"total_usd"`
	RefundedUSD       *int64 `bun:"refunded_usd"`
	Fee               int64  `bun:"fee,notnull"`
	FeeUSD            *int64 `bun:"fee_usd"`
	FeeIsEstimated    bool   `bun:"fee_is_estimated,notnull"`
	// FXRate is the USD per unit of Currency the *USD amounts used, as a
	// decimal; FXSource identity | ecb | polar; all nil when not converted or
	// converted by the provider (Lemon Squeezy).
	FXRate            *string    `bun:"fx_rate,type:numeric"`
	FXSource          *string    `bun:"fx_source"`
	FXDate            *time.Time `bun:"fx_date,type:date"`
	Status            string     `bun:"status,notnull"`
	OccurredAt        time.Time  `bun:"occurred_at,notnull"`
	ProviderUpdatedAt time.Time  `bun:"provider_updated_at,notnull"`
	CreatedAt         time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt         time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
}

// FXRate is one European Central Bank reference rate: the units of Currency
// one euro bought on RateDate. Global, not tenant scoped.
type FXRate struct {
	bun.BaseModel `bun:"table:fx_rates,alias:fx" swaggerignore:"true"`

	RateDate  time.Time `bun:"rate_date,pk,type:date"`
	Currency  string    `bun:"currency,pk"`
	PerEUR    string    `bun:"per_eur,notnull,type:numeric"`
	FetchedAt time.Time `bun:"fetched_at,notnull,default:current_timestamp"`
}
