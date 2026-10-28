package lemonsqueezy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// Payments. An order (a checkout) and a subscription invoice (every
// subscription charge, the first included) are the objects that move money.
// Each becomes a charge payment and, once refunded, a refund payment holding
// the amount refunded so far. Lemon Squeezy reports no chargebacks and no
// fees.

// paymentsPageSize is the API's largest page.
const paymentsPageSize = 100

// moneyStatuses are the statuses of an order or invoice that took money.
// pending, failed, void and fraudulent ones moved nothing.
var moneyStatuses = map[string]bool{"paid": true, "refunded": true, "partial_refund": true}

// paymentAttrs is the subset of an order or subscription invoice we keep.
type paymentAttrs struct {
	Currency          string     `json:"currency"`
	Subtotal          int64      `json:"subtotal"`
	DiscountTotal     int64      `json:"discount_total"`
	Tax               int64      `json:"tax"`
	Total             int64      `json:"total"`
	SubtotalUSD       int64      `json:"subtotal_usd"`
	DiscountTotalUSD  int64      `json:"discount_total_usd"`
	TaxUSD            int64      `json:"tax_usd"`
	TotalUSD          int64      `json:"total_usd"`
	RefundedAmount    int64      `json:"refunded_amount"`
	RefundedAmountUSD int64      `json:"refunded_amount_usd"`
	RefundedAt        flexTime   `json:"refunded_at"`
	Status            string     `json:"status"`
	TestMode          bool       `json:"test_mode"`
	CreatedAt         flexTime   `json:"created_at"`
	UpdatedAt         flexTime   `json:"updated_at"`
	SubscriptionID    flexString `json:"subscription_id"`
	BillingReason     string     `json:"billing_reason"`
	FirstOrderItem    *struct {
		VariantID flexString `json:"variant_id"`
	} `json:"first_order_item"`
}

// decodePayments reads one order or subscription invoice. It returns nothing
// for one that took no money.
func decodePayments(source, id string, raw json.RawMessage) ([]billing.Payment, error) {
	var a paymentAttrs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("lemonsqueezy: decode %s %s: %w", source, id, err)
	}
	if !moneyStatuses[a.Status] {
		return nil, nil
	}
	if a.CreatedAt.t == nil {
		return nil, fmt.Errorf("lemonsqueezy: %s %s has no created_at", source, id)
	}
	charge := billing.Payment{
		Source: source, ProviderID: id, Kind: source, TestMode: a.TestMode,
		SubscriptionID: string(a.SubscriptionID), BillingReason: a.BillingReason, Currency: a.Currency,
		Subtotal: a.Subtotal, Discount: a.DiscountTotal, Tax: a.Tax, Total: a.Total, Refunded: a.RefundedAmount,
		SubtotalUSD: a.SubtotalUSD, DiscountUSD: a.DiscountTotalUSD, TaxUSD: a.TaxUSD, TotalUSD: a.TotalUSD, RefundedUSD: a.RefundedAmountUSD,
		Status: a.Status, OccurredAt: *a.CreatedAt.t, UpdatedAt: *a.CreatedAt.t,
	}
	if a.UpdatedAt.t != nil {
		charge.UpdatedAt = *a.UpdatedAt.t
	}
	if a.FirstOrderItem != nil {
		charge.VariantID = string(a.FirstOrderItem.VariantID)
	}
	out := []billing.Payment{charge}
	if a.RefundedAmount <= 0 {
		return out, nil
	}
	// The refund is the amount refunded so far. Its tax is the refunded share
	// of the charge's tax; the provider does not split it.
	refund := charge
	refund.Kind = billing.PaymentKindRefund
	refund.Total, refund.TotalUSD = a.RefundedAmount, a.RefundedAmountUSD
	refund.Tax = billing.Share(a.Tax, a.RefundedAmount, a.Total)
	refund.TaxUSD = billing.Share(a.TaxUSD, a.RefundedAmountUSD, a.TotalUSD)
	refund.Subtotal, refund.SubtotalUSD = refund.Total-refund.Tax, refund.TotalUSD-refund.TaxUSD
	refund.Discount, refund.DiscountUSD = 0, 0
	refund.Refunded, refund.RefundedUSD = 0, 0
	refund.OccurredAt = charge.UpdatedAt
	if a.RefundedAt.t != nil {
		refund.OccurredAt = *a.RefundedAt.t
	}
	return append(out, refund), nil
}

// paymentPaths are the list endpoints by source.
var paymentPaths = map[string]string{
	billing.PaymentSourceOrder:               "/orders",
	billing.PaymentSourceSubscriptionInvoice: "/subscription-invoices",
}

// ListPayments reads one page of a store's orders or subscription invoices
// (page numbers start at 1), in the mode of the client's key.
func (c *Client) ListPayments(ctx context.Context, storeID, source string, page int) (billing.PaymentPage, error) {
	path, ok := paymentPaths[source]
	if !ok {
		return billing.PaymentPage{}, fmt.Errorf("lemonsqueezy: unknown payment source %q", source)
	}
	q := url.Values{}
	q.Set("filter[store_id]", storeID)
	q.Set("page[number]", strconv.Itoa(max(page, 1)))
	q.Set("page[size]", strconv.Itoa(paymentsPageSize))
	var doc struct {
		Data []resource `json:"data"`
		Meta struct {
			Page struct {
				LastPage int `json:"lastPage"`
			} `json:"page"`
		} `json:"meta"`
	}
	if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &doc); err != nil {
		return billing.PaymentPage{}, err
	}
	out := billing.PaymentPage{LastPage: doc.Meta.Page.LastPage}
	for _, r := range doc.Data {
		ps, err := decodePayments(source, string(r.ID), r.Attributes)
		if err != nil {
			return billing.PaymentPage{}, err
		}
		out.Payments = append(out.Payments, ps...)
	}
	return out, nil
}
