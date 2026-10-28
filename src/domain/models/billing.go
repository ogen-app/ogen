package models

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// BillingProviderLemonSqueezy is the only billing provider today.
const BillingProviderLemonSqueezy = "lemonsqueezy"

// Subscription statuses, in the provider's own vocabulary. The column has no
// CHECK: an unknown value from the provider is stored as-is.
const (
	SubscriptionStatusOnTrial   = "on_trial"
	SubscriptionStatusActive    = "active"
	SubscriptionStatusPaused    = "paused"
	SubscriptionStatusPastDue   = "past_due"
	SubscriptionStatusUnpaid    = "unpaid"
	SubscriptionStatusCancelled = "cancelled"
	SubscriptionStatusExpired   = "expired"
)

// InvalidatedReasonModeCutover marks a test-mode subscription invalidated at
// the sandbox-to-live cutover.
const InvalidatedReasonModeCutover = "mode_cutover"

// BillingSubscription mirrors one provider subscription of a workspace.
type BillingSubscription struct {
	bun.BaseModel `bun:"table:billing_subscriptions,alias:bs" swaggerignore:"true"`

	ID                     string     `bun:"id,pk"`
	TenantID               string     `bun:"tenant_id,notnull"`
	AccountID              *string    `bun:"account_id"`
	Provider               string     `bun:"provider,notnull"`
	ProviderSubscriptionID string     `bun:"provider_subscription_id,notnull"`
	ProviderCustomerID     string     `bun:"provider_customer_id,notnull"`
	ProviderOrderID        string     `bun:"provider_order_id,notnull"`
	ProviderVariantID      string     `bun:"provider_variant_id,notnull"`
	TierVersionPriceID     *string    `bun:"tier_version_price_id"`
	Status                 string     `bun:"status,notnull"`
	TestMode               bool       `bun:"test_mode,notnull"`
	RenewsAt               *time.Time `bun:"renews_at"`
	EndsAt                 *time.Time `bun:"ends_at"`
	TrialEndsAt            *time.Time `bun:"trial_ends_at"`
	FirstPaidAt            *time.Time `bun:"first_paid_at"`
	CardBrand              string     `bun:"card_brand,notnull"`
	CardLastFour           string     `bun:"card_last_four,notnull"`
	ProviderUpdatedAt      time.Time  `bun:"provider_updated_at,notnull"`
	InvalidatedAt          *time.Time `bun:"invalidated_at"`
	InvalidatedReason      string     `bun:"invalidated_reason,notnull"`
	TeardownAt             *time.Time `bun:"teardown_at"`
	CreatedAt              time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt              time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
}

// Webhook ingest outcomes.
const (
	WebhookOutcomeApplied        = "applied"
	WebhookOutcomeStale          = "stale"
	WebhookOutcomeIgnoredMode    = "ignored_mode"
	WebhookOutcomeIgnoredUnknown = "ignored_unknown"
	WebhookOutcomeFailed         = "failed"
)

// BillingWebhookEvent is one provider webhook delivery. BodySHA256 is the
// dedupe key (the provider sends no event id).
type BillingWebhookEvent struct {
	bun.BaseModel `bun:"table:billing_webhook_events,alias:bwe" swaggerignore:"true"`

	ID               string          `bun:"id,pk"`
	Provider         string          `bun:"provider,notnull"`
	EventName        string          `bun:"event_name,notnull"`
	BodySHA256       string          `bun:"body_sha256,notnull"`
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

// BillingScheduledChange is a downgrade landing at the billing boundary.
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

// Who set a variant mapping.
const (
	VariantMappingByHarbor  = "harbor"
	VariantMappingByEnvSeed = "env-seed"
)

// BillingVariantMapping ties a tier version price row to the provider variant
// that sells it, separately per mode. An operator-wide row, not tenant scoped.
type BillingVariantMapping struct {
	bun.BaseModel `bun:"table:billing_variant_mappings,alias:bvm" swaggerignore:"true"`

	TierVersionPriceID string    `bun:"tier_version_price_id,pk"                     json:"tier_version_price_id"`
	TestMode           bool      `bun:"test_mode,pk"                                 json:"test_mode"`
	VariantID          string    `bun:"variant_id,notnull"                           json:"variant_id"`
	UpdatedAt          time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	UpdatedBy          string    `bun:"updated_by,notnull"                           json:"updated_by"`
	Reason             string    `bun:"reason,notnull"                               json:"reason"`
}

// BillingPayment is one provider money movement: an order, a subscription
// invoice, or the refund of one. Amounts are in minor units, with *USD in US
// cents. TenantID is the checkout's custom data when the payload carried it.
type BillingPayment struct {
	bun.BaseModel `bun:"table:billing_payments,alias:bp" swaggerignore:"true"`

	ID                     string    `bun:"id,pk"`
	Provider               string    `bun:"provider,notnull"`
	Source                 string    `bun:"source,notnull"`
	ProviderID             string    `bun:"provider_id,notnull"`
	Kind                   string    `bun:"kind,notnull"`
	TestMode               bool      `bun:"test_mode,notnull"`
	TenantID               *string   `bun:"tenant_id"`
	ProviderSubscriptionID string    `bun:"provider_subscription_id,notnull"`
	ProviderVariantID      string    `bun:"provider_variant_id,notnull"`
	BillingReason          string    `bun:"billing_reason,notnull"`
	Currency               string    `bun:"currency,notnull"`
	Subtotal               int64     `bun:"subtotal,notnull"`
	Discount               int64     `bun:"discount,notnull"`
	Tax                    int64     `bun:"tax,notnull"`
	Total                  int64     `bun:"total,notnull"`
	Refunded               int64     `bun:"refunded,notnull"`
	SubtotalUSD            int64     `bun:"subtotal_usd,notnull"`
	DiscountUSD            int64     `bun:"discount_usd,notnull"`
	TaxUSD                 int64     `bun:"tax_usd,notnull"`
	TotalUSD               int64     `bun:"total_usd,notnull"`
	RefundedUSD            int64     `bun:"refunded_usd,notnull"`
	Fee                    int64     `bun:"fee,notnull"`
	FeeUSD                 int64     `bun:"fee_usd,notnull"`
	FeeIsEstimated         bool      `bun:"fee_is_estimated,notnull"`
	Status                 string    `bun:"status,notnull"`
	OccurredAt             time.Time `bun:"occurred_at,notnull"`
	ProviderUpdatedAt      time.Time `bun:"provider_updated_at,notnull"`
	CreatedAt              time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt              time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}
