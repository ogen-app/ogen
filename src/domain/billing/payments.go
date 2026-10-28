package billing

import "time"

// Payment kinds: what a money movement is. A refund or chargeback carries the
// id of the order or invoice it reverses.
const (
	PaymentKindOrder               = "order"
	PaymentKindSubscriptionInvoice = "subscription_invoice"
	PaymentKindRefund              = "refund"
	PaymentKindChargeback          = "chargeback"
)

// Payment sources: the provider object a payment was read from. Order and
// invoice ids are separate id spaces, so the source is part of a payment's
// identity.
const (
	PaymentSourceOrder               = "order"
	PaymentSourceSubscriptionInvoice = "subscription_invoice"
)

// Payment is one money movement at the provider. Amounts are in minor units
// of Currency and positive for every kind; the *USD fields are the provider's
// conversion to US cents at the time of the payment.
type Payment struct {
	Source     string
	ProviderID string
	Kind       string
	TestMode   bool
	// TenantID is the checkout's custom data, when the payload carried it.
	TenantID       string
	SubscriptionID string
	VariantID      string
	// BillingReason is initial | renewal | updated for an invoice.
	BillingReason string
	Currency      string

	Subtotal, Discount, Tax, Total, Refunded                int64
	SubtotalUSD, DiscountUSD, TaxUSD, TotalUSD, RefundedUSD int64

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

// FeeFormula estimates the provider's fee, which its payloads do not report:
// a percentage of the USD total plus a fixed USD amount per charge.
type FeeFormula struct {
	// PercentBPS is the percentage in basis points (500 = 5%).
	PercentBPS int64
	// FixedUSDCents is charged once per payment.
	FixedUSDCents int64
}

// Estimate returns the fee for a payment in its currency and in US cents. A
// refund or chargeback, and a charge of nothing (a free trial), carry no fee:
// the provider keeps the fee of a refunded charge.
func (f FeeFormula) Estimate(p Payment) (fee, feeUSD int64) {
	if p.TotalUSD <= 0 || (p.Kind != PaymentKindOrder && p.Kind != PaymentKindSubscriptionInvoice) {
		return 0, 0
	}
	feeUSD = roundDiv(p.TotalUSD*f.PercentBPS, 10_000) + f.FixedUSDCents
	// The fee in the payment's currency, at the payment's own rate.
	return roundDiv(feeUSD*p.Total, p.TotalUSD), feeUSD
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
