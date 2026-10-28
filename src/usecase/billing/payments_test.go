package billing

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

// paymentEvent delivers an order_* or subscription_payment_* webhook carrying
// payments.
func (h *harness) paymentEvent(name, objectID string, custom map[string]string, payments ...billingdom.Payment) error {
	ev := billingdom.Event{Name: name, Kind: billingdom.EventOther, TestMode: true, ObjectID: objectID, CustomData: custom, Payments: payments}
	if payments[0].Source == billingdom.PaymentSourceSubscriptionInvoice {
		ev.Kind, ev.PaymentSubscriptionID, ev.PaymentAmountMinor = billingdom.EventPayment, payments[0].SubscriptionID, payments[0].Total
	}
	body, err := json.Marshal(ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.svc.HandleWebhook(h.t.Context(), "ok", body)
}

func (h *harness) listPayments(testMode bool, pageSize int, token string) ([]PaymentRow, string) {
	h.t.Helper()
	rows, next, err := h.svc.ListPayments(h.t.Context(), testMode, nil, nil, pageSize, token)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows, next
}

func charge(source, id string, total int64, at time.Time) billingdom.Payment {
	return billingdom.Payment{
		Source: source, ProviderID: id, Kind: source, TestMode: true, Currency: "eur",
		Subtotal: total, Total: total, SubtotalUSD: total + total/10, TotalUSD: total + total/10,
		Status: "paid", OccurredAt: at, UpdatedAt: at,
	}
}

func TestWebhookPaymentsAreRecordedOnceAndListed(t *testing.T) {
	h := newHarness(t)
	h.svc.d.Fees = billingdom.FeeFormula{PercentBPS: 500, FixedUSDCents: 50}
	tenantID, _, accountID := h.workspace("a")
	custom := map[string]string{"tenant_id": tenantID, "account_id": accountID}

	// The checkout: an order, the subscription it opened, and that
	// subscription's initial invoice, which is the same charge.
	day1 := h.now.Add(-48 * time.Hour)
	snap := h.sub("s1", proVariant, models.SubscriptionStatusActive, day1)
	snap.OrderID = "o1"
	if err := h.webhook("subscription_created", snap, custom); err != nil {
		t.Fatal(err)
	}
	order := charge(billingdom.PaymentSourceOrder, "o1", 2900, day1)
	order.VariantID = proVariant
	if err := h.paymentEvent("order_created", "o1", custom, order); err != nil {
		t.Fatal(err)
	}
	initial := charge(billingdom.PaymentSourceSubscriptionInvoice, "i1", 2900, day1)
	initial.SubscriptionID, initial.BillingReason = "s1", "initial"
	for range 2 { // a replayed webhook adds nothing
		if err := h.paymentEvent("subscription_payment_success", "i1", nil, initial); err != nil {
			t.Fatal(err)
		}
	}
	// A one-off order with no subscription behind it.
	day2 := h.now.Add(-24 * time.Hour)
	if err := h.paymentEvent("order_created", "o2", custom, charge(billingdom.PaymentSourceOrder, "o2", 500, day2)); err != nil {
		t.Fatal(err)
	}

	rows, next := h.listPayments(true, 0, "")
	if next != "" || len(rows) != 2 {
		t.Fatalf("rows = %+v, want the initial invoice and the one-off order", rows)
	}
	inv, oneOff := rows[0], rows[1]
	if inv.ProviderID != "i1" || inv.Kind != billingdom.PaymentKindSubscriptionInvoice || inv.ResolvedTenantID != tenantID ||
		inv.TierID != h.proPrice.TierID || inv.Currency != "EUR" || inv.FeeUSD != 210 || inv.Fee != 191 || !inv.FeeIsEstimated {
		t.Fatalf("invoice = %+v", inv)
	}
	if oneOff.ProviderID != "o2" || oneOff.ResolvedTenantID != tenantID || oneOff.TierID != "" {
		t.Fatalf("one-off order = %+v", oneOff)
	}

	// The invoice is partly refunded: the charge is updated and the refund
	// becomes its own row. An older copy delivered late changes nothing.
	day3 := h.now
	refunded := initial
	refunded.Status, refunded.Refunded, refunded.UpdatedAt = "partial_refund", 1000, day3
	refund := refunded
	refund.Kind, refund.Total, refund.Subtotal, refund.Refunded, refund.OccurredAt = billingdom.PaymentKindRefund, 1000, 1000, 0, day3
	if err := h.paymentEvent("subscription_payment_refunded", "i1", nil, refunded, refund); err != nil {
		t.Fatal(err)
	}
	if err := h.paymentEvent("subscription_payment_success", "i1", nil, initial); err != nil {
		t.Fatal(err)
	}
	rows, _ = h.listPayments(true, 0, "")
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want the refund listed too", rows)
	}
	if rows[0].Status != "partial_refund" || rows[0].Refunded != 1000 {
		t.Fatalf("refunded invoice = %+v", rows[0])
	}
	if r := rows[2]; r.Kind != billingdom.PaymentKindRefund || r.Total != 1000 || r.Fee != 0 || r.ResolvedTenantID != tenantID || !r.OccurredAt.Equal(day3) {
		t.Fatalf("refund = %+v", r)
	}

	// Live mode is apart.
	if live, _ := h.listPayments(false, 0, ""); len(live) != 0 {
		t.Fatalf("live rows = %+v", live)
	}
}

func TestListPaymentsPagesAndBounds(t *testing.T) {
	h := newHarness(t)
	var batch []billingdom.Payment
	for i := range 5 {
		batch = append(batch, charge(billingdom.PaymentSourceOrder, string(rune('a'+i)), 100, h.now.Add(time.Duration(i)*time.Hour)))
	}
	if err := h.svc.RecordPayments(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	var got []string
	token := ""
	for {
		rows, next := h.listPayments(true, 2, token)
		for _, r := range rows {
			got = append(got, r.ProviderID)
		}
		if next == "" {
			break
		}
		token = next
	}
	if len(got) != 5 || got[0] != "a" || got[4] != "e" {
		t.Fatalf("paged = %v", got)
	}

	from, to := h.now.Add(time.Hour), h.now.Add(3*time.Hour)
	rows, _, err := h.svc.ListPayments(t.Context(), true, &from, &to, 0, "")
	if err != nil || len(rows) != 2 || rows[0].ProviderID != "b" || rows[1].ProviderID != "c" {
		t.Fatalf("[from, to) = %+v %v", rows, err)
	}
	if _, _, err := h.svc.ListPayments(t.Context(), true, nil, nil, 0, "not-a-token"); !errors.Is(err, ErrBadPageToken) {
		t.Fatalf("bad token: %v", err)
	}
}
