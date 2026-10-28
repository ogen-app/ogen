package lemonsqueezy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// A partly refunded €12.10 order: €10.00 + €2.10 VAT, €6.05 refunded.
const partlyRefundedOrder = `{
	"store_id": 77, "customer_id": 9, "currency": "EUR",
	"subtotal": 1000, "discount_total": 0, "tax": 210, "total": 1210,
	"subtotal_usd": 1080, "discount_total_usd": 0, "tax_usd": 227, "total_usd": 1307,
	"refunded_amount": 605, "refunded_amount_usd": 654, "refunded_at": "2026-09-20T08:00:00.000000Z",
	"status": "partial_refund", "test_mode": true,
	"created_at": "2026-09-03T10:00:00.000000Z", "updated_at": "2026-09-20T08:00:01.000000Z",
	"first_order_item": {"variant_id": 555}
}`

func TestDecodePayments_OrderWithRefund(t *testing.T) {
	got, err := decodePayments(billing.PaymentSourceOrder, "4001", []byte(partlyRefundedOrder))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("payments = %+v, want the order and its refund", got)
	}
	order, refund := got[0], got[1]
	if order.Kind != billing.PaymentKindOrder || order.ProviderID != "4001" || order.VariantID != "555" || !order.TestMode ||
		order.Total != 1210 || order.TotalUSD != 1307 || order.Refunded != 605 || order.RefundedUSD != 654 ||
		!order.OccurredAt.Equal(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)) ||
		!order.UpdatedAt.Equal(time.Date(2026, 9, 20, 8, 0, 1, 0, time.UTC)) {
		t.Fatalf("order = %+v", order)
	}
	if refund.Kind != billing.PaymentKindRefund || refund.ProviderID != "4001" || refund.Source != billing.PaymentSourceOrder ||
		refund.Total != 605 || refund.Tax != 105 || refund.Subtotal != 500 || refund.Refunded != 0 ||
		refund.TotalUSD != 654 || refund.TaxUSD != 114 || refund.SubtotalUSD != 540 ||
		!refund.OccurredAt.Equal(time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("refund = %+v", refund)
	}
}

func TestDecodePayments_InvoiceAndStatuses(t *testing.T) {
	invoice := `{"subscription_id": 31, "billing_reason": "renewal", "currency": "USD", "subtotal": 2900, "tax": 0, "total": 2900,
		"subtotal_usd": 2900, "total_usd": 2900, "status": "paid", "created_at": "2026-10-01T00:00:00Z"}`
	got, err := decodePayments(billing.PaymentSourceSubscriptionInvoice, "88", []byte(invoice))
	if err != nil || len(got) != 1 {
		t.Fatalf("invoice = %+v %v", got, err)
	}
	if p := got[0]; p.Kind != billing.PaymentKindSubscriptionInvoice || p.SubscriptionID != "31" || p.BillingReason != "renewal" || p.TestMode ||
		!p.UpdatedAt.Equal(p.OccurredAt) {
		t.Fatalf("invoice payment = %+v", p)
	}
	for _, status := range []string{"pending", "failed", "void", "fraudulent"} {
		got, err := decodePayments(billing.PaymentSourceOrder, "1", []byte(`{"status":"`+status+`","created_at":"2026-10-01T00:00:00Z"}`))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %+v %v, want nothing", status, got, err)
		}
	}
	if _, err := decodePayments(billing.PaymentSourceOrder, "1", []byte(`{"status":"paid"}`)); err == nil {
		t.Error("a paid order with no created_at decoded")
	}
}

func TestListPayments(t *testing.T) {
	var query url.Values
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(`{"meta":{"page":{"currentPage":2,"lastPage":3}},"data":[
			{"type":"orders","id":"4001","attributes":` + partlyRefundedOrder + `},
			{"type":"orders","id":"4002","attributes":{"status":"failed","created_at":"2026-09-04T00:00:00Z"}}]}`))
	}))
	t.Cleanup(srv.Close)
	c := New(Options{APIKey: static("key-1"), BaseURL: srv.URL, Timeout: time.Second})

	page, err := c.ListPayments(t.Context(), "77", billing.PaymentSourceOrder, 2)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/orders" || query.Get("filter[store_id]") != "77" || query.Get("page[number]") != "2" || query.Get("page[size]") != "100" {
		t.Fatalf("request %s?%s", path, query.Encode())
	}
	if page.LastPage != 3 || len(page.Payments) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if _, err := c.ListPayments(t.Context(), "77", "refunds", 1); err == nil {
		t.Fatal("unknown source listed")
	}
}

func TestParseWebhook_Payments(t *testing.T) {
	body := `{"meta":{"event_name":"order_refunded","custom_data":{"tenant_id":"t1"}},
		"data":{"type":"orders","id":"4001","attributes":` + partlyRefundedOrder + `}}`
	ev, err := parseWebhook([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != billing.EventOther || len(ev.Payments) != 2 || ev.CustomData["tenant_id"] != "t1" {
		t.Fatalf("event = %+v", ev)
	}

	invoice := `{"meta":{"event_name":"subscription_payment_success"},"data":{"type":"subscription-invoices","id":"88","attributes":
		{"subscription_id":31,"status":"paid","total":2900,"total_usd":2900,"currency":"USD","created_at":"2026-10-01T00:00:00Z"}}}`
	ev, err = parseWebhook([]byte(invoice))
	if err != nil || ev.Kind != billing.EventPayment || ev.PaymentSubscriptionID != "31" || len(ev.Payments) != 1 {
		t.Fatalf("invoice event = %+v %v", ev, err)
	}

	// A payment that does not decode never blocks the event itself.
	odd := `{"meta":{"event_name":"subscription_payment_success"},"data":{"type":"subscription-invoices","id":"89","attributes":
		{"subscription_id":31,"status":"paid","total":2900}}}`
	ev, err = parseWebhook([]byte(odd))
	if err != nil || ev.Kind != billing.EventPayment || len(ev.Payments) != 0 {
		t.Fatalf("odd invoice event = %+v %v", ev, err)
	}
}
