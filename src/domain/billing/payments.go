package billing

import "time"

// Payment kinds: what a money movement is. Lemon Squeezy history also has
// subscription invoices and chargebacks.
const (
	PaymentKindOrder               = "order"
	PaymentKindSubscriptionInvoice = "subscription_invoice"
	PaymentKindRefund              = "refund"
	PaymentKindChargeback          = "chargeback"
)

// Payment sources: the provider object a payment was read from. Each source
// is its own id space, so the source is part of a payment's identity.
const (
	PaymentSourceOrder = "order"
	// PaymentSourceSubscriptionInvoice is Lemon Squeezy history only.
	PaymentSourceSubscriptionInvoice = "subscription_invoice"
	PaymentSourceRefund              = "refund"
)

// Payment is one money movement at the provider. Amounts are in minor units
// of Currency and positive for every kind.
type Payment struct {
	Source     string
	ProviderID string
	Kind       string
	TestMode   bool
	// TenantID is the customer's external id or the checkout's metadata,
	// when the payload carried either.
	TenantID       string
	SubscriptionID string
	ProductID      string
	// BillingReason is initial | renewal | updated for a subscription charge.
	BillingReason string
	Currency      string

	Subtotal, Discount, Tax, Total, Refunded int64
	// Fee is the provider's own fee in Currency, when the payload reports
	// it; nil means it is estimated.
	Fee *int64

	Status     string
	OccurredAt time.Time
	// UpdatedAt is the provider's last change: the newer copy wins.
	UpdatedAt time.Time
}

// PaymentPage is one page of a provider's payment history.
type PaymentPage struct {
	Payments []Payment
	// LastPage is the provider's last page number; the page read is the
	// last when it is reached.
	LastPage int
}

// FeeFormula estimates the provider's fee when its payloads do not report
// it: a percentage of the USD total plus a fixed USD amount per charge.
type FeeFormula struct {
	// PercentBPS is the percentage in basis points (500 = 5%).
	PercentBPS int64
	// FixedUSDCents is charged once per payment.
	FixedUSDCents int64
}

// Estimate returns the fee for a payment in its currency and in US cents,
// from its total in both. A refund or chargeback, and a charge of nothing (a
// free trial), carry no fee: the provider keeps the fee of a refunded charge.
func (f FeeFormula) Estimate(kind string, total, totalUSD int64) (fee, feeUSD int64) {
	if totalUSD <= 0 || (kind != PaymentKindOrder && kind != PaymentKindSubscriptionInvoice) {
		return 0, 0
	}
	feeUSD = roundDiv(totalUSD*f.PercentBPS, 10_000) + f.FixedUSDCents
	// The fee in the payment's currency, at the payment's own rate.
	return roundDiv(feeUSD*total, totalUSD), feeUSD
}

// Share is part of whole in proportion to of/total, rounded half away from
// zero: the tax inside a partial refund, say. A zero total shares nothing.
func Share(part, of, total int64) int64 {
	if total == 0 {
		return 0
	}
	return roundDiv(part*of, total)
}

// roundDiv divides rounding half away from zero; d must be positive.
func roundDiv(n, d int64) int64 {
	if n < 0 {
		return -roundDiv(-n, d)
	}
	return (n + d/2) / d
}
