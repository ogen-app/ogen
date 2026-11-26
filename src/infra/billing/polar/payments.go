package polar

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// Payments: Polar has one order per charge and each refund is its own
// object, so a payment is an order or a refund, read from those two lists.

// orderJSON is the subset of the order object a payment needs.
type orderJSON struct {
	ID             string        `json:"id"`
	CreatedAt      time.Time     `json:"created_at"`
	ModifiedAt     *time.Time    `json:"modified_at"`
	Status         string        `json:"status"`
	SubtotalAmount int64         `json:"subtotal_amount"`
	DiscountAmount int64         `json:"discount_amount"`
	TaxAmount      int64         `json:"tax_amount"`
	TotalAmount    int64         `json:"total_amount"`
	RefundedAmount int64         `json:"refunded_amount"`
	Currency       string        `json:"currency"`
	BillingReason  string        `json:"billing_reason"`
	SubscriptionID *string       `json:"subscription_id"`
	ProductID      *string       `json:"product_id"`
	Customer       *customerJSON `json:"customer"`
	Metadata       metadata      `json:"metadata"`
	// PlatformFeeAmount is Polar's fee, in PlatformFeeCurrency, which may
	// differ from the order's (a USD fee on a EUR order).
	PlatformFeeAmount   *int64  `json:"platform_fee_amount"`
	PlatformFeeCurrency *string `json:"platform_fee_currency"`
}

// refundJSON is the subset of the refund object a payment needs.
type refundJSON struct {
	ID             string     `json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	ModifiedAt     *time.Time `json:"modified_at"`
	Status         string     `json:"status"`
	Amount         int64      `json:"amount"`
	TaxAmount      int64      `json:"tax_amount"`
	Currency       string     `json:"currency"`
	OrderID        string     `json:"order_id"`
	SubscriptionID *string    `json:"subscription_id"`
	Metadata       metadata   `json:"metadata"`
}

// Polar's order billing reasons, and Ogen's names for them.
var billingReasons = map[string]string{
	"purchase":            "initial",
	"subscription_create": "initial",
	"subscription_cycle":  "renewal",
	"subscription_update": "updated",
}

// chargesSubscription reports whether an order is a subscription's own
// charge (its first or a renewal), which proves the workspace paid.
func (o orderJSON) chargesSubscription() bool {
	return o.BillingReason == "subscription_create" || o.BillingReason == "subscription_cycle"
}

// orderStatuses maps the order statuses that are money movements; pending and
// void orders are not stored.
var orderStatuses = map[string]string{"paid": "paid", "refunded": "refunded", "partially_refunded": "partial_refund"}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (o orderJSON) payment(testMode bool) (billing.Payment, bool) {
	status, ok := orderStatuses[o.Status]
	if !ok || o.ID == "" {
		return billing.Payment{}, false
	}
	p := billing.Payment{
		Source: billing.PaymentSourceOrder, ProviderID: o.ID, Kind: billing.PaymentKindOrder, TestMode: testMode,
		TenantID:       cmp.Or(o.Customer.externalID(), o.Metadata["tenant_id"]),
		SubscriptionID: str(o.SubscriptionID), ProductID: str(o.ProductID), BillingReason: billingReasons[o.BillingReason],
		Currency: strings.ToUpper(o.Currency),
		Subtotal: o.SubtotalAmount, Discount: o.DiscountAmount, Tax: o.TaxAmount, Total: o.TotalAmount, Refunded: o.RefundedAmount,
		Fee: o.PlatformFeeAmount, FeeCurrency: strings.ToUpper(str(o.PlatformFeeCurrency)),
		Status: status, OccurredAt: o.CreatedAt.UTC(), UpdatedAt: o.CreatedAt.UTC(),
	}
	if o.ModifiedAt != nil {
		p.UpdatedAt = o.ModifiedAt.UTC()
	}
	return p, true
}

// payment is a refund that went through; pending, failed and canceled ones
// moved no money. The refund's amount excludes its tax (spike).
func (r refundJSON) payment(testMode bool) (billing.Payment, bool) {
	if r.Status != "succeeded" || r.ID == "" {
		return billing.Payment{}, false
	}
	p := billing.Payment{
		Source: billing.PaymentSourceRefund, ProviderID: r.ID, Kind: billing.PaymentKindRefund, TestMode: testMode,
		TenantID: r.Metadata["tenant_id"], SubscriptionID: str(r.SubscriptionID), Currency: strings.ToUpper(r.Currency),
		Subtotal: r.Amount, Tax: r.TaxAmount, Total: r.Amount + r.TaxAmount,
		Status: "refunded", OccurredAt: r.CreatedAt.UTC(), UpdatedAt: r.CreatedAt.UTC(),
	}
	if r.ModifiedAt != nil {
		p.UpdatedAt = r.ModifiedAt.UTC()
	}
	return p, true
}

// PaymentSources implements billing.Provider.
func (c *Client) PaymentSources() []string {
	return []string{billing.PaymentSourceOrder, billing.PaymentSourceRefund}
}

// ListPayments implements billing.Provider: one page of the running mode's
// orders or refunds, oldest first.
func (c *Client) ListPayments(ctx context.Context, source string, pageNo int) (billing.PaymentPage, error) {
	q := listQuery(c.orgID, max(pageNo, 1), url.Values{"sorting": {"created_at"}})
	switch source {
	case billing.PaymentSourceOrder:
		var pg page[orderJSON]
		if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodGet, path: "/orders/", query: q}, &pg); err != nil {
			return billing.PaymentPage{}, err
		}
		out := billing.PaymentPage{LastPage: max(pg.Pagination.MaxPage, 1)}
		for _, o := range pg.Items {
			if p, ok := o.payment(c.testMode); ok {
				out.Payments = append(out.Payments, p)
			}
		}
		return out, nil
	case billing.PaymentSourceRefund:
		var pg page[refundJSON]
		if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodGet, path: "/refunds/", query: q}, &pg); err != nil {
			return billing.PaymentPage{}, err
		}
		out := billing.PaymentPage{LastPage: max(pg.Pagination.MaxPage, 1)}
		for _, r := range pg.Items {
			if p, ok := r.payment(c.testMode); ok {
				out.Payments = append(out.Payments, p)
			}
		}
		return out, nil
	default:
		return billing.PaymentPage{}, fmt.Errorf("polar: unknown payment source %q", source)
	}
}

func decodeOrder(raw json.RawMessage) (orderJSON, error) {
	var o orderJSON
	if err := json.Unmarshal(raw, &o); err != nil {
		return orderJSON{}, fmt.Errorf("polar: decode order: %w", err)
	}
	return o, nil
}

func decodeRefund(raw json.RawMessage) (refundJSON, error) {
	var r refundJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return refundJSON{}, fmt.Errorf("polar: decode refund: %w", err)
	}
	return r, nil
}
