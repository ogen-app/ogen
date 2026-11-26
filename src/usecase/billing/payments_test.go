package billing

import (
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

// paymentEvent delivers an order or refund webhook carrying payments. A paid
// subscription charge also tells the subscription it was paid.
func (h *harness) paymentEvent(name, objectID string, metadata map[string]string, payments ...billingdom.Payment) error {
	ev := billingdom.Event{Name: name, Kind: billingdom.EventPayment, TestMode: true, ObjectID: objectID, Metadata: metadata, Payments: payments,
		PaymentSubscriptionID: payments[0].SubscriptionID}
	ev.PaymentCharged = name == "order.paid" && payments[0].SubscriptionID != "" && payments[0].Total > 0
	return h.deliver(ev)
}

func (h *harness) listPayments(testMode bool, pageSize int, token string) ([]PaymentRow, string) {
	h.t.Helper()
	rows, next, err := h.svc.ListPayments(h.t.Context(), testMode, nil, nil, pageSize, token)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows, next
}

func order(id, currency string, total int64, at time.Time) billingdom.Payment {
	return billingdom.Payment{
		Source: billingdom.PaymentSourceOrder, ProviderID: id, Kind: billingdom.PaymentKindOrder, TestMode: true, Currency: currency,
		Subtotal: total, Total: total, Status: "paid", OccurredAt: at, UpdatedAt: at,
	}
}

// sameRate compares a stored decimal rate with want by value: the column
// keeps ten decimal places.
func sameRate(got *string, want string) bool {
	if got == nil {
		return false
	}
	g, err1 := billingdom.ParseRate(*got)
	w, err2 := billingdom.ParseRate(want)
	return err1 == nil && err2 == nil && g.Cmp(w) == 0
}

// storeRates records EUR-based rates for a day: 1 EUR = usd USD.
func (h *harness) storeRates(day time.Time, usd string) {
	h.t.Helper()
	r, ok := new(big.Rat).SetString(usd)
	if !ok {
		h.t.Fatal("bad rate")
	}
	if err := h.svc.RecordFXRates(h.t.Context(), []billingdom.FXRate{{Date: day, Currency: "USD", PerEUR: r}}); err != nil {
		h.t.Fatal(err)
	}
}

func TestWebhookPaymentsAreRecordedOnceAndListed(t *testing.T) {
	h := newHarness(t)
	h.svc.d.Fees = billingdom.FeeFormula{PercentBPS: 500, FixedUSDCents: 50}
	tenantID, _, accountID := h.workspace("a")
	meta := map[string]string{"tenant_id": tenantID, "account_id": accountID}

	day1 := h.now.Add(-48 * time.Hour)
	h.storeRates(day1, "1.1")
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, day1)
	if err := h.webhook("subscription.created", snap, meta); err != nil {
		t.Fatal(err)
	}
	// The subscription's first charge, in EUR, converted at day1's rate.
	first := order("o1", "eur", 2900, day1)
	first.SubscriptionID, first.ProductID, first.BillingReason = "s1", proProduct, "initial"
	for range 2 { // a replayed delivery adds nothing
		if err := h.deliverAs("evt_o1", billingdom.Event{Name: "order.paid", Kind: billingdom.EventPayment, TestMode: true, ObjectID: "o1",
			Metadata: meta, PaymentSubscriptionID: "s1", PaymentCharged: true, Payments: []billingdom.Payment{first}}); err != nil {
			t.Fatal(err)
		}
	}
	if m := h.mirror("s1"); m.FirstPaidAt == nil {
		t.Fatal("the paid charge did not mark the subscription paid")
	}
	// A one-off USD order with no subscription behind it.
	day2 := h.now.Add(-24 * time.Hour)
	if err := h.paymentEvent("order.paid", "o2", meta, order("o2", "USD", 500, day2)); err != nil {
		t.Fatal(err)
	}

	rows, next := h.listPayments(true, 0, "")
	if next != "" || len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two orders", rows)
	}
	charge, oneOff := rows[0], rows[1]
	if charge.ProviderID != "o1" || charge.ResolvedTenantID != tenantID || charge.TierID != h.proPrice.TierID || charge.Currency != "EUR" ||
		charge.ProviderProductID != proProduct || charge.ProviderVariantID != proProduct {
		t.Fatalf("charge = %+v", charge)
	}
	// 29.00 EUR at 1.1 = 31.90 USD; fee 5% + 50¢ = 2.10 USD = 1.91 EUR.
	if charge.TotalUSD == nil || *charge.TotalUSD != 3190 || *charge.FeeUSD != 210 || charge.Fee != 191 || !charge.FeeIsEstimated ||
		*charge.FXSource != billingdom.FXSourceECB || !sameRate(charge.FXRate, "1.1") || !charge.FXDate.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("charge conversion = %+v", charge)
	}
	if oneOff.ProviderID != "o2" || oneOff.TierID != "" || *oneOff.TotalUSD != 500 || *oneOff.FXSource != billingdom.FXSourceIdentity || oneOff.FXDate != nil {
		t.Fatalf("one-off order = %+v", oneOff)
	}

	// The charge is partly refunded: the order is updated and the refund is
	// its own row. An older copy delivered late changes nothing.
	day3 := h.now
	h.storeRates(day3, "1.2")
	refunded := first
	refunded.Status, refunded.Refunded, refunded.UpdatedAt = "partial_refund", 1000, day3
	if err := h.paymentEvent("order.refunded", "o1", meta, refunded); err != nil {
		t.Fatal(err)
	}
	refund := billingdom.Payment{Source: billingdom.PaymentSourceRefund, ProviderID: "r1", Kind: billingdom.PaymentKindRefund, TestMode: true,
		SubscriptionID: "s1", Currency: "EUR", Subtotal: 1000, Total: 1000, Status: "refunded", OccurredAt: day3, UpdatedAt: day3}
	if err := h.paymentEvent("refund.created", "r1", nil, refund); err != nil {
		t.Fatal(err)
	}
	if err := h.paymentEvent("order.paid", "o1", meta, first); err != nil {
		t.Fatal(err)
	}
	rows, _ = h.listPayments(true, 0, "")
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want the refund listed too", rows)
	}
	if rows[0].Status != "partial_refund" || rows[0].Refunded != 1000 || *rows[0].RefundedUSD != 1100 {
		t.Fatalf("refunded order = %+v", rows[0])
	}
	if r := rows[2]; r.Kind != billingdom.PaymentKindRefund || r.Total != 1000 || r.Fee != 0 || r.ResolvedTenantID != tenantID || *r.TotalUSD != 1200 {
		t.Fatalf("refund = %+v", r)
	}

	// Live mode is apart.
	if live, _ := h.listPayments(false, 0, ""); len(live) != 0 {
		t.Fatalf("live rows = %+v", live)
	}
}

func TestPaymentsConvertOnceTheRateIsKnown(t *testing.T) {
	h := newHarness(t)
	h.svc.d.Fees = billingdom.FeeFormula{PercentBPS: 500, FixedUSDCents: 50}
	ctx := t.Context()
	day := h.now.Add(-24 * time.Hour)
	p := order("o1", "EUR", 1000, day)

	// No rate for the day yet: stored, unconverted.
	if err := h.svc.RecordPayments(ctx, []billingdom.Payment{p}); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.listPayments(true, 0, "")
	if len(rows) != 1 || rows[0].TotalUSD != nil || rows[0].FeeUSD != nil || rows[0].FXSource != nil || rows[0].Fee != 0 {
		t.Fatalf("unconverted = %+v", rows)
	}

	// The next sync, with the rate stored, converts it.
	h.storeRates(day, "1.05")
	if err := h.svc.RecordPayments(ctx, []billingdom.Payment{p}); err != nil {
		t.Fatal(err)
	}
	rows, _ = h.listPayments(true, 0, "")
	if *rows[0].TotalUSD != 1050 || *rows[0].FeeUSD != 103 || rows[0].Fee != 98 {
		t.Fatalf("converted = %+v", rows[0])
	}

	// A later copy that cannot be converted keeps the conversion.
	if _, err := h.db.NewDelete().Model((*models.FXRate)(nil)).Where("true").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RecordPayments(ctx, []billingdom.Payment{p}); err != nil {
		t.Fatal(err)
	}
	rows, _ = h.listPayments(true, 0, "")
	if rows[0].TotalUSD == nil || *rows[0].TotalUSD != 1050 || rows[0].Fee != 98 || !sameRate(rows[0].FXRate, "1.05") {
		t.Fatalf("conversion lost: %+v", rows[0])
	}

	// A fee the provider reports is stored as is.
	fee := int64(80)
	p.ProviderID, p.Fee = "o2", &fee
	h.storeRates(day, "1.05")
	if err := h.svc.RecordPayments(ctx, []billingdom.Payment{p}); err != nil {
		t.Fatal(err)
	}
	rows, _ = h.listPayments(true, 0, "")
	if r := rows[slices.IndexFunc(rows, func(r PaymentRow) bool { return r.ProviderID == "o2" })]; r.Fee != 80 || r.FeeIsEstimated || *r.FeeUSD != 84 {
		t.Fatalf("reported fee = %+v", r)
	}
}

func TestListPaymentsPagesAndBounds(t *testing.T) {
	h := newHarness(t)
	var batch []billingdom.Payment
	for i := range 5 {
		batch = append(batch, order(string(rune('a'+i)), "USD", 100, h.now.Add(time.Duration(i)*time.Hour)))
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
