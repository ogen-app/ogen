package polar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

var now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func static(v string) SecretResolver {
	return func(context.Context) (string, error) { return v, nil }
}

// recorded is one request the test server saw.
type recorded struct {
	method, path, query, auth, version string
	body                               map[string]any
}

// server answers requests by "METHOD path" and records them.
func server(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) (*Client, *[]recorded) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []recorded
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), version: r.Header.Get("Polar-Version")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		mu.Lock()
		seen = append(seen, rec)
		mu.Unlock()
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"ResourceNotFound","detail":"Not found"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{
		AccessToken: static("polar_oat_test"), LiveAccessToken: static("polar_oat_live"), WebhookSecret: static("secret"),
		OrganizationID: "org_sandbox", LiveOrganizationID: "org_live", TestMode: true, APIVersion: "2026-10",
		BaseURL: srv.URL, Now: func() time.Time { return now },
	})
	return c, &seen
}

func reply(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func fail(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestMapStatus(t *testing.T) {
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		status string
		cancel bool
		ended  *time.Time
		want   string
	}{
		{"incomplete", false, nil, ""},
		{"incomplete_expired", false, nil, ""},
		{"trialing", false, nil, models.SubscriptionStatusOnTrial},
		{"trialing", true, nil, models.SubscriptionStatusCancelled},
		{"active", false, nil, models.SubscriptionStatusActive},
		{"active", true, nil, models.SubscriptionStatusCancelled},
		{"past_due", false, nil, models.SubscriptionStatusPastDue},
		{"unpaid", false, nil, models.SubscriptionStatusUnpaid},
		{"paused", false, nil, models.SubscriptionStatusPaused},
		{"canceled", false, &future, models.SubscriptionStatusCancelled},
		{"canceled", false, &past, models.SubscriptionStatusExpired},
		{"canceled", false, nil, models.SubscriptionStatusExpired},
		{"trial_ended", false, nil, models.SubscriptionStatusExpired},
		{"something_new", false, nil, ""},
	}
	for _, c := range cases {
		if got := MapStatus(c.status, c.cancel, c.ended, now); got != c.want {
			t.Errorf("MapStatus(%s, cancel=%v, ended=%v) = %q, want %q", c.status, c.cancel, c.ended, got, c.want)
		}
	}
}

const subscriptionJSONBody = `{
	"id": "sub_1", "created_at": "2026-11-01T10:00:00Z", "modified_at": "2026-11-20T10:00:00.123456Z",
	"status": "active", "currency": "eur", "amount": 2900, "recurring_interval": "month",
	"current_period_start": "2026-11-01T10:00:00Z", "current_period_end": "2026-12-01T10:00:00Z",
	"trial_start": null, "trial_end": null, "cancel_at_period_end": false, "canceled_at": null, "ends_at": null, "ended_at": null,
	"customer_id": "cus_1", "product_id": "prod_max",
	"customer": {"id": "cus_1", "external_id": "t-1", "email": "o@example.com"},
	"metadata": {"tenant_id": "t-1", "account_id": "a-1", "n": 3, "flag": true},
	"pending_update": {"id": "pu_1", "product_id": "prod_pro", "applies_at": "2026-12-01T10:00:00Z"}
}`

func TestDecodeSubscription(t *testing.T) {
	c := New(Options{TestMode: true, Now: func() time.Time { return now }})
	s, err := c.decodeSubscription(json.RawMessage(subscriptionJSONBody))
	if err != nil {
		t.Fatal(err)
	}
	renews := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	if s.ID != "sub_1" || s.CustomerID != "cus_1" || s.CustomerExternalID != "t-1" || s.ProductID != "prod_max" || s.Currency != "eur" ||
		s.Status != models.SubscriptionStatusActive || s.ProviderStatus != "active" || !s.TestMode ||
		s.RenewsAt == nil || !s.RenewsAt.Equal(renews) || s.EndsAt != nil || s.TrialEndsAt != nil {
		t.Fatalf("subscription = %+v", s)
	}
	if s.PendingProductID != "prod_pro" || s.PendingEffectiveAt == nil || !s.PendingEffectiveAt.Equal(renews) {
		t.Fatalf("pending = %s %v", s.PendingProductID, s.PendingEffectiveAt)
	}
	if !s.UpdatedAt.Equal(time.Date(2026, 11, 20, 10, 0, 0, 123456000, time.UTC)) {
		t.Fatalf("updated = %v", s.UpdatedAt)
	}
	if s.Metadata["tenant_id"] != "t-1" || s.Metadata["n"] != "3" || s.Metadata["flag"] != "true" {
		t.Fatalf("metadata = %v", s.Metadata)
	}

	// Cancelled at period end: live until the period ends, nothing renews.
	cancelled := strings.Replace(subscriptionJSONBody, `"cancel_at_period_end": false`, `"cancel_at_period_end": true`, 1)
	s, _ = c.decodeSubscription(json.RawMessage(cancelled))
	if s.Status != models.SubscriptionStatusCancelled || s.RenewsAt != nil || s.EndsAt == nil || !s.EndsAt.Equal(renews) || !s.CancelAtPeriodEnd {
		t.Fatalf("cancelled = %+v", s)
	}
	// A trial ends at trial_end.
	trial := strings.NewReplacer(`"status": "active"`, `"status": "trialing"`, `"trial_end": null`, `"trial_end": "2026-12-01T10:00:00Z"`).Replace(subscriptionJSONBody)
	s, _ = c.decodeSubscription(json.RawMessage(trial))
	if s.Status != models.SubscriptionStatusOnTrial || s.TrialEndsAt == nil || !s.TrialEndsAt.Equal(renews) {
		t.Fatalf("trial = %+v", s)
	}
	// A pending update back to the current product is no change.
	same := strings.Replace(subscriptionJSONBody, `"product_id": "prod_pro"`, `"product_id": "prod_max"`, 1)
	if s, _ := c.decodeSubscription(json.RawMessage(same)); s.PendingProductID != "" {
		t.Fatalf("pending to the same product = %q", s.PendingProductID)
	}
}

func TestClientRequestsAndErrors(t *testing.T) {
	c, seen := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /checkouts/":         reply(`{"id":"co_1","url":"https://sandbox.polar.sh/checkout/co_1","expires_at":"2026-11-26T13:00:00Z"}`),
		"GET /subscriptions/sub_1": reply(subscriptionJSONBody),
		"PATCH /subscriptions/sub_1": func(w http.ResponseWriter, r *http.Request) {
			reply(subscriptionJSONBody)(w, r)
		},
		"PATCH /subscriptions/sub_declined": fail(http.StatusPaymentRequired, `{"error":"PaymentFailed","detail":"card declined"}`),
		"PATCH /subscriptions/sub_locked":   fail(http.StatusConflict, `{"error":"SubscriptionLocked","detail":"busy"}`),
		"PATCH /subscriptions/sub_gone":     fail(http.StatusForbidden, `{"error":"AlreadyCanceled","detail":"cancelled"}`),
		"PATCH /subscriptions/sub_bad":      fail(http.StatusUnprocessableEntity, `{"detail":[{"loc":["body","product_id"],"msg":"invalid","type":"value_error"}]}`),
		"POST /customer-sessions/":          reply(`{"token":"x","customer_portal_url":"https://sandbox.polar.sh/portal?t=x","expires_at":"2026-11-26T13:00:00Z"}`),
	})
	ctx := context.Background()

	co, err := c.CreateCheckout(ctx, billing.CheckoutRequest{ProductID: "prod_pro", CustomerExternalID: "t-1", Email: "o@example.com",
		Metadata: map[string]string{"tenant_id": "t-1"}, SuccessURL: "https://app/plans", SkipTrial: true})
	if err != nil || co.URL == "" || !co.ExpiresAt.Equal(time.Date(2026, 11, 26, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("checkout = %+v %v", co, err)
	}
	req := (*seen)[0]
	if req.auth != "Bearer polar_oat_test" || req.version != "2026-10" || req.body["external_customer_id"] != "t-1" || req.body["allow_trial"] != false ||
		req.body["allow_discount_codes"] != false || req.body["success_url"] != "https://app/plans" || req.body["customer_email"] != "o@example.com" {
		t.Fatalf("checkout request = %+v", req)
	}
	if products, _ := req.body["products"].([]any); len(products) != 1 || products[0] != "prod_pro" {
		t.Fatalf("checkout products = %v", req.body["products"])
	}

	for timing, want := range map[billing.ChangeTiming]string{billing.ChargeNow: "invoice", billing.NextPeriod: "next_period", billing.NoCharge: "prorate"} {
		if _, err := c.ChangePlan(ctx, "sub_1", "prod_pro", billing.ChangeOpts{Timing: timing}); err != nil {
			t.Fatal(err)
		}
		last := (*seen)[len(*seen)-1]
		if last.body["proration_behavior"] != want || last.body["product_id"] != "prod_pro" || last.body["trial_end"] != nil {
			t.Fatalf("change %v = %v", timing, last.body)
		}
	}
	if _, err := c.ChangePlan(ctx, "sub_1", "prod_pro", billing.ChangeOpts{EndTrialNow: true}); err != nil {
		t.Fatal(err)
	}
	if last := (*seen)[len(*seen)-1]; last.body["trial_end"] != "now" {
		t.Fatalf("end trial = %v", last.body)
	}
	if _, err := c.CancelScheduledChange(ctx, "sub_1"); err != nil {
		t.Fatal(err)
	}
	if last := (*seen)[len(*seen)-1]; len(last.body) != 1 || last.body["pending_update"] != nil {
		t.Fatalf("cancel scheduled change = %v", last.body)
	} else if _, ok := last.body["pending_update"]; !ok {
		t.Fatalf("cancel scheduled change sent no pending_update: %v", last.body)
	}

	if _, err := c.GetSubscription(ctx, "sub_nope"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("404 = %v", err)
	}
	if _, err := c.ChangePlan(ctx, "sub_declined", "p", billing.ChangeOpts{}); !errors.Is(err, billing.ErrPaymentFailed) || billing.IsRetryable(err) {
		t.Fatalf("402 = %v", err)
	}
	_, err = c.ChangePlan(ctx, "sub_locked", "p", billing.ChangeOpts{})
	if apiErr, ok := errors.AsType[*APIError](err); !ok || !apiErr.Locked() || !billing.IsRetryable(err) {
		t.Fatalf("409 locked = %v", err)
	}
	if _, err := c.ChangePlan(ctx, "sub_gone", "p", billing.ChangeOpts{}); !errors.Is(err, billing.ErrAlreadyCanceled) {
		t.Fatalf("403 = %v", err)
	}
	_, err = c.ChangePlan(ctx, "sub_bad", "p", billing.ChangeOpts{})
	if apiErr, ok := errors.AsType[*APIError](err); !ok || apiErr.Status != 422 || !strings.Contains(apiErr.Detail, "invalid") || billing.IsRetryable(err) {
		t.Fatalf("422 = %v", err)
	}

	session, err := c.PortalSession(ctx, "t-1")
	if err != nil || !strings.Contains(session.URL, "portal") || session.ExpiresAt.IsZero() {
		t.Fatalf("portal = %+v %v", session, err)
	}
	if last := (*seen)[len(*seen)-1]; last.body["external_customer_id"] != "t-1" {
		t.Fatalf("portal request = %v", last.body)
	}

	// No token: not configured, nothing sent.
	unset := New(Options{AccessToken: static(""), TestMode: true})
	if _, err := unset.GetSubscription(ctx, "sub_1"); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("no token = %v", err)
	}
}

func TestCatalogPagesAndReadsLiveWithItsToken(t *testing.T) {
	page1 := `{"items":[
		{"id":"prod_pro","name":"Pro monthly","is_recurring":true,"is_archived":false,"recurring_interval":"month","trial_interval":"month","trial_interval_count":1,
		 "prices":[{"amount_type":"fixed","price_amount":2900,"price_currency":"eur","is_archived":false},
		           {"amount_type":"fixed","price_amount":3200,"price_currency":"usd","is_archived":false},
		           {"amount_type":"fixed","price_amount":1,"price_currency":"gbp","is_archived":true},
		           {"amount_type":"custom","price_currency":"eur","is_archived":true}]},
		{"id":"prod_team","name":"Team monthly","is_recurring":true,"is_archived":false,"recurring_interval":"month",
		 "prices":[{"amount_type":"fixed","price_amount":5000,"price_currency":"eur","is_archived":false},
		           {"amount_type":"seat_based","price_currency":"eur","is_archived":false}]}
	],"pagination":{"total_count":3,"max_page":2}}`
	page2 := `{"items":[{"id":"prod_old","name":"Old","is_recurring":true,"is_archived":true,"recurring_interval":"year","prices":[]}],"pagination":{"total_count":2,"max_page":2}}`
	c, seen := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /products/": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				reply(page2)(w, r)
				return
			}
			reply(page1)(w, r)
		},
	})
	cat, err := c.Catalog(context.Background(), true)
	if err != nil || len(cat.Products) != 3 {
		t.Fatalf("catalog = %+v %v", cat, err)
	}
	pro := cat.Products[0]
	if amount, ok := pro.PriceIn("USD"); !ok || amount != 3200 || len(pro.Prices) != 2 || !pro.OneMonthTrial() || !pro.TestMode || pro.IntervalCount != 1 || pro.VariablePricing {
		t.Fatalf("pro = %+v", pro)
	}
	// A seat price is billed too, so the product is not a fixed-price one.
	if team := cat.Products[1]; !team.VariablePricing || len(team.Prices) != 1 {
		t.Fatalf("team = %+v", team)
	}
	if old := cat.Products[2]; !old.Archived || old.Interval != "year" {
		t.Fatalf("old = %+v", old)
	}
	if q := (*seen)[0].query; !strings.Contains(q, "organization_id=org_sandbox") || !strings.Contains(q, "is_recurring=true") || !strings.Contains(q, "limit=100") {
		t.Fatalf("query = %s", q)
	}

	// Production is read with the live token and organization, flagged live.
	cat, err = c.Catalog(context.Background(), false)
	last := (*seen)[len(*seen)-1]
	if err != nil || cat.TestMode || cat.Products[0].TestMode || last.auth != "Bearer polar_oat_live" || !strings.Contains(last.query, "organization_id=org_live") {
		t.Fatalf("live catalog = %+v %v, request %+v", cat, err, last)
	}
}

func TestListPayments(t *testing.T) {
	c, _ := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /orders/": reply(`{"items":[
			{"id":"o1","created_at":"2026-11-01T10:00:00Z","modified_at":"2026-11-02T10:00:00Z","status":"partially_refunded",
			 "subtotal_amount":2900,"discount_amount":0,"tax_amount":551,"total_amount":3451,"refunded_amount":1000,"currency":"eur",
			 "billing_reason":"subscription_cycle","subscription_id":"sub_1","product_id":"prod_pro","platform_fee_amount":195,
			 "customer":{"id":"cus_1","external_id":"t-1"},"metadata":{}},
			{"id":"o2","created_at":"2026-11-01T10:00:00Z","status":"pending","total_amount":100,"currency":"eur"}
		],"pagination":{"total_count":2,"max_page":3}}`),
		"GET /refunds/": reply(`{"items":[
			{"id":"r1","created_at":"2026-11-03T10:00:00Z","status":"succeeded","amount":840,"tax_amount":160,"currency":"eur","order_id":"o1","subscription_id":"sub_1","metadata":{}},
			{"id":"r2","created_at":"2026-11-03T10:00:00Z","status":"failed","amount":5,"currency":"eur","order_id":"o1"}
		],"pagination":{"total_count":2,"max_page":1}}`),
	})
	ctx := context.Background()
	if got := c.PaymentSources(); len(got) != 2 || got[0] != billing.PaymentSourceOrder || got[1] != billing.PaymentSourceRefund {
		t.Fatalf("sources = %v", got)
	}
	orders, err := c.ListPayments(ctx, billing.PaymentSourceOrder, 1)
	if err != nil || orders.LastPage != 3 || len(orders.Payments) != 1 {
		t.Fatalf("orders = %+v %v", orders, err)
	}
	o := orders.Payments[0]
	if o.ProviderID != "o1" || o.Kind != billing.PaymentKindOrder || o.Status != "partial_refund" || o.Currency != "EUR" || o.Total != 3451 || o.Tax != 551 ||
		o.Refunded != 1000 || o.BillingReason != "renewal" || o.TenantID != "t-1" || o.SubscriptionID != "sub_1" || o.ProductID != "prod_pro" ||
		o.Fee == nil || *o.Fee != 195 || !o.UpdatedAt.Equal(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) || !o.TestMode {
		t.Fatalf("order = %+v", o)
	}
	refunds, err := c.ListPayments(ctx, billing.PaymentSourceRefund, 1)
	if err != nil || len(refunds.Payments) != 1 {
		t.Fatalf("refunds = %+v %v", refunds, err)
	}
	if r := refunds.Payments[0]; r.ProviderID != "r1" || r.Source != billing.PaymentSourceRefund || r.Kind != billing.PaymentKindRefund || r.Total != 1000 || r.Tax != 160 || r.SubscriptionID != "sub_1" {
		t.Fatalf("refund = %+v", r)
	}
	if _, err := c.ListPayments(ctx, "invoices", 1); err == nil {
		t.Fatal("unknown source listed")
	}
}

func TestWebhookVerify(t *testing.T) {
	body := []byte(`{"type":"subscription.updated","data":{}}`)
	c := New(Options{WebhookSecret: static("secret"), Now: func() time.Time { return now }})
	ctx := context.Background()
	h := Sign("secret", "evt_1", now, body)
	if err := c.VerifyWebhook(ctx, h, body); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	// Several signatures, one valid (a rotation in flight).
	multi := h
	multi.Signature = "v1,bm9wZQ== " + h.Signature
	if err := c.VerifyWebhook(ctx, multi, body); err != nil {
		t.Fatalf("multi-signature: %v", err)
	}
	for name, bad := range map[string]billing.WebhookHeaders{
		"tampered id":  {ID: "evt_2", Timestamp: h.Timestamp, Signature: h.Signature},
		"old":          Sign("secret", "evt_1", now.Add(-10*time.Minute), body),
		"future":       Sign("secret", "evt_1", now.Add(10*time.Minute), body),
		"other secret": Sign("other", "evt_1", now, body),
		"no headers":   {},
		"v2 only":      {ID: h.ID, Timestamp: h.Timestamp, Signature: strings.Replace(h.Signature, "v1,", "v2,", 1)},
	} {
		if err := c.VerifyWebhook(ctx, bad, body); !errors.Is(err, billing.ErrInvalidSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := c.VerifyWebhook(ctx, h, []byte(`{"tampered":true}`)); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Errorf("tampered body: %v", err)
	}

	// A Standard Webhooks secret: whsec_ + base64 of the key.
	key := []byte("raw-key-bytes")
	whsec := "whsec_" + base64.StdEncoding.EncodeToString(key)
	std := New(Options{WebhookSecret: static(whsec), Now: func() time.Time { return now }})
	if err := std.VerifyWebhook(ctx, Sign(string(key), "evt_1", now, body), body); err != nil {
		t.Fatalf("whsec_ secret: %v", err)
	}
	// The same whsec_ string used as raw bytes (Polar's own secrets) too.
	if err := std.VerifyWebhook(ctx, Sign(whsec, "evt_1", now, body), body); err != nil {
		t.Fatalf("whsec_ secret used raw: %v", err)
	}

	unset := New(Options{WebhookSecret: static("")})
	if err := unset.VerifyWebhook(ctx, h, body); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("no secret: %v", err)
	}
}

func TestParseWebhook(t *testing.T) {
	c := New(Options{TestMode: true, Now: func() time.Time { return now }})
	h := billing.WebhookHeaders{ID: "evt_1"}

	ev, err := c.ParseWebhook(h, []byte(`{"type":"subscription.updated","timestamp":"2026-11-26T12:00:00Z","data":`+subscriptionJSONBody+`}`))
	if err != nil || ev.ID != "evt_1" || ev.Kind != billing.EventSubscription || ev.ObjectID != "sub_1" || ev.Subscription == nil ||
		ev.CustomerExternalID != "t-1" || ev.Metadata["account_id"] != "a-1" || !ev.TestMode {
		t.Fatalf("subscription event = %+v %v", ev, err)
	}

	order := `{"type":"order.paid","data":{"id":"o1","created_at":"2026-11-01T10:00:00Z","status":"paid","total_amount":2900,"subtotal_amount":2900,
		"currency":"eur","billing_reason":"subscription_create","subscription_id":"sub_1","customer":{"id":"c","external_id":"t-1"},"metadata":{"tenant_id":"t-1"}}}`
	ev, err = c.ParseWebhook(h, []byte(order))
	if err != nil || ev.Kind != billing.EventPayment || !ev.PaymentCharged || ev.PaymentSubscriptionID != "sub_1" || len(ev.Payments) != 1 || ev.Payments[0].BillingReason != "initial" {
		t.Fatalf("order event = %+v %v", ev, err)
	}
	// A trial's $0 first order is not a charge.
	free := strings.Replace(order, `"total_amount":2900`, `"total_amount":0`, 1)
	if ev, _ := c.ParseWebhook(h, []byte(free)); ev.PaymentCharged {
		t.Fatal("a zero order counted as a charge")
	}
	// order.updated of a paid order records it but is not the charge.
	if ev, _ := c.ParseWebhook(h, []byte(strings.Replace(order, "order.paid", "order.updated", 1))); ev.PaymentCharged || len(ev.Payments) != 1 {
		t.Fatalf("order.updated = %+v", ev)
	}

	refund := `{"type":"refund.created","data":{"id":"r1","created_at":"2026-11-03T10:00:00Z","status":"succeeded","amount":1000,"tax_amount":0,"currency":"eur","order_id":"o1","subscription_id":"sub_1"}}`
	if ev, err := c.ParseWebhook(h, []byte(refund)); err != nil || ev.Kind != billing.EventPayment || ev.PaymentCharged || len(ev.Payments) != 1 || ev.Payments[0].Kind != billing.PaymentKindRefund {
		t.Fatalf("refund event = %+v %v", ev, err)
	}
	if ev, err := c.ParseWebhook(h, []byte(`{"type":"checkout.expired","data":{"id":"co_1"}}`)); err != nil || ev.Kind != billing.EventOther || ev.ObjectID != "co_1" {
		t.Fatalf("other event = %+v %v", ev, err)
	}
	if _, err := c.ParseWebhook(h, []byte(`{"data":{}}`)); err == nil {
		t.Fatal("event without a type parsed")
	}
}

func TestRemotePlanAndDecode(t *testing.T) {
	sandbox := New(Options{TestMode: true, APIVersion: "2026-10", OrganizationID: "org_1"})
	if sandbox.Upstream() != UpstreamSandbox || New(Options{}).Upstream() != UpstreamProduction {
		t.Fatal("upstreams")
	}
	read := func(p billing.Provider) error { _, err := p.GetSubscription(context.Background(), "sub_1"); return err }
	call, err := sandbox.Plan(read)
	if err != nil || call.Method != http.MethodGet || call.Path != "/subscriptions/sub_1" || call.Headers["Polar-Version"] != "2026-10" {
		t.Fatalf("plan = %+v %v", call, err)
	}
	pay := func(p billing.Provider) error {
		_, err := p.ListPayments(context.Background(), billing.PaymentSourceOrder, 2)
		return err
	}
	if call, _ := sandbox.Plan(pay); !strings.HasPrefix(call.Path, "/orders/?") || !strings.Contains(call.Path, "organization_id=org_1") || !strings.Contains(call.Path, "page=2") {
		t.Fatalf("payments plan = %+v", call)
	}

	var got billing.Subscription
	err = sandbox.Decode(billing.CallResult{Status: 200, Body: []byte(subscriptionJSONBody)}, func(p billing.Provider) error {
		var err error
		got, err = p.GetSubscription(context.Background(), "sub_1")
		return err
	})
	if err != nil || got.ID != "sub_1" || !got.TestMode {
		t.Fatalf("decode = %+v %v", got, err)
	}
	if err := sandbox.Decode(billing.CallResult{Status: 404, Body: []byte(`{}`)}, read); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("decode 404 = %v", err)
	}
	if err := sandbox.Decode(billing.CallResult{Err: "dial tcp: timeout"}, read); err == nil || !billing.IsRetryable(err) {
		t.Fatalf("decode transport error = %v", err)
	}
}
