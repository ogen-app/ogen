package lemonsqueezy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

func static(v string) SecretResolver {
	return func(context.Context) (string, error) { return v, nil }
}

type recorded struct {
	method, path, auth, contentType string
	body                            map[string]any
}

func server(t *testing.T, status int, reply string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path = r.Method, r.URL.Path
		rec.auth, rec.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return New(Options{APIKey: static("key-1"), WebhookSecret: static("whsec"), StoreID: "77", TestMode: true, BaseURL: srv.URL, Timeout: time.Second}), rec
}

func TestCreateCheckout(t *testing.T) {
	c, rec := server(t, http.StatusCreated, `{"data":{"type":"checkouts","id":"abc","attributes":{"url":"https://store.lemonsqueezy.com/checkout/x","expires_at":"2026-10-21T13:00:00.000000Z"}}}`)
	exp := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	got, err := c.CreateCheckout(t.Context(), billing.CheckoutRequest{
		VariantID: "555", Email: "o@example.com", Custom: map[string]string{"tenant_id": "t1"},
		RedirectURL: "https://app/plans", ExpiresAt: exp, TestMode: true, SkipTrial: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://store.lemonsqueezy.com/checkout/x" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("session = %+v", got)
	}
	if rec.method != http.MethodPost || rec.path != "/checkouts" || rec.auth != "Bearer key-1" || rec.contentType != mediaType {
		t.Fatalf("request = %+v", rec)
	}
	data := rec.body["data"].(map[string]any)
	attrs := data["attributes"].(map[string]any)
	if attrs["test_mode"] != true || attrs["checkout_options"].(map[string]any)["skip_trial"] != true {
		t.Fatalf("attributes = %v", attrs)
	}
	if attrs["checkout_data"].(map[string]any)["custom"].(map[string]any)["tenant_id"] != "t1" {
		t.Fatalf("custom = %v", attrs["checkout_data"])
	}
	rels := data["relationships"].(map[string]any)
	if rels["store"].(map[string]any)["data"].(map[string]any)["id"] != "77" || rels["variant"].(map[string]any)["data"].(map[string]any)["id"] != "555" {
		t.Fatalf("relationships = %v", rels)
	}
}

const subscriptionJSON = `{"data":{"type":"subscriptions","id":"9","attributes":{
 "customer_id":12,"order_id":34,"variant_id":555,"status":"on_trial","card_brand":"visa","card_last_four":"4242",
 "trial_ends_at":"2026-11-21T10:00:00.000000Z","renews_at":"2026-11-21T10:00:00.000000Z","ends_at":null,
 "updated_at":"2026-10-21T10:00:01.000000Z","test_mode":true,
 "urls":{"customer_portal":"https://store/billing?signature=x"},"unknown_attr":{"nested":1}}}}`

func TestGetSubscription(t *testing.T) {
	c, rec := server(t, http.StatusOK, subscriptionJSON)
	s, err := c.GetSubscription(t.Context(), "9")
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/subscriptions/9" {
		t.Fatalf("path = %s", rec.path)
	}
	if s.ID != "9" || s.CustomerID != "12" || s.OrderID != "34" || s.VariantID != "555" || s.Status != "on_trial" ||
		!s.TestMode || s.CardBrand != "visa" || s.CardLastFour != "4242" || s.EndsAt != nil || s.TrialEndsAt == nil ||
		s.PortalURL == "" || s.UpdatedAt.IsZero() {
		t.Fatalf("subscription = %+v", s)
	}
}

func TestChangePlan(t *testing.T) {
	c, rec := server(t, http.StatusOK, subscriptionJSON)
	if _, err := c.ChangePlan(t.Context(), "9", "556", billing.ChangeOpts{InvoiceNow: true, EndTrialNow: true}); err != nil {
		t.Fatal(err)
	}
	attrs := rec.body["data"].(map[string]any)["attributes"].(map[string]any)
	if rec.method != http.MethodPatch || attrs["variant_id"] != float64(556) || attrs["invoice_immediately"] != true || attrs["trial_ends_at"] == nil {
		t.Fatalf("patch = %s %v", rec.method, attrs)
	}
	if _, ok := attrs["disable_prorations"]; ok {
		t.Fatal("disable_prorations sent with InvoiceNow")
	}

	if _, err := c.ChangePlan(t.Context(), "9", "556", billing.ChangeOpts{NoProration: true}); err != nil {
		t.Fatal(err)
	}
	attrs = rec.body["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["disable_prorations"] != true || attrs["invoice_immediately"] != nil || attrs["trial_ends_at"] != nil {
		t.Fatalf("no-proration patch = %v", attrs)
	}
	if _, err := c.ChangePlan(t.Context(), "9", "abc", billing.ChangeOpts{}); err == nil {
		t.Fatal("non-numeric variant accepted")
	}
}

func TestGetVariant(t *testing.T) {
	c, _ := server(t, http.StatusOK, `{"data":{"type":"variants","id":"555","attributes":{"price":2900,"interval":"month","has_free_trial":true,"trial_interval":"month","trial_interval_count":1,"test_mode":true}}}`)
	v, err := c.GetVariant(t.Context(), "555")
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "555" || v.PriceMinor != 2900 || v.Interval != "month" || !v.OneMonthTrial() || !v.TestMode {
		t.Fatalf("variant = %+v", v)
	}
}

func TestErrors(t *testing.T) {
	c, _ := server(t, http.StatusUnprocessableEntity, `{"errors":[{"title":"Unprocessable","detail":"variant_id is invalid"}]}`)
	_, err := c.GetSubscription(t.Context(), "9")
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok || apiErr.Status != 422 || apiErr.Detail != "variant_id is invalid" || IsRetryable(err) {
		t.Fatalf("422 err = %v", err)
	}
	if strings.Contains(err.Error(), "key-1") {
		t.Fatal("api key leaked into the error")
	}

	c, _ = server(t, http.StatusServiceUnavailable, `oops`)
	if _, err := c.GetSubscription(t.Context(), "9"); !IsRetryable(err) {
		t.Fatalf("503 not retryable: %v", err)
	}
	c, _ = server(t, http.StatusNotFound, `{}`)
	if _, err := c.GetSubscription(t.Context(), "9"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}

	unconfigured := New(Options{APIKey: static(""), WebhookSecret: static(""), StoreID: "77", BaseURL: "http://127.0.0.1:1", Timeout: time.Second})
	if _, err := unconfigured.GetSubscription(t.Context(), "9"); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("no key err = %v", err)
	}
	if err := unconfigured.VerifyWebhook(t.Context(), "aa", []byte("{}")); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("no secret err = %v", err)
	}
}

func TestVerifyWebhook(t *testing.T) {
	c := New(Options{APIKey: static("k"), WebhookSecret: static("whsec"), StoreID: "77"})
	body := []byte(`{"meta":{"event_name":"subscription_created"}}`)
	if err := c.VerifyWebhook(t.Context(), Sign("whsec", body), body); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	for _, sig := range []string{Sign("other", body), "", "zz", Sign("whsec", body)[:10]} {
		if err := c.VerifyWebhook(t.Context(), sig, body); !errors.Is(err, billing.ErrInvalidSignature) {
			t.Fatalf("signature %q: %v", sig, err)
		}
	}
}

func TestParseWebhook(t *testing.T) {
	sub := `{"meta":{"event_name":"subscription_created","test_mode":true,"custom_data":{"tenant_id":"t1","user_id":42}},` +
		strings.TrimPrefix(subscriptionJSON, "{")
	ev, err := parseWebhook([]byte(sub))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != billing.EventSubscription || ev.Name != "subscription_created" || !ev.TestMode || ev.ObjectID != "9" ||
		ev.CustomData["tenant_id"] != "t1" || ev.CustomData["user_id"] != "42" || ev.Subscription == nil ||
		ev.Subscription.VariantID != "555" || ev.Subscription.PortalURL != "" {
		t.Fatalf("subscription event = %+v %+v", ev, ev.Subscription)
	}

	pay := `{"meta":{"event_name":"subscription_payment_success"},"data":{"type":"subscription-invoices","id":"70","attributes":{"subscription_id":9,"total":2900,"status":"paid","test_mode":false}}}`
	ev, err = parseWebhook([]byte(pay))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != billing.EventPayment || ev.PaymentSubscriptionID != "9" || ev.PaymentAmountMinor != 2900 || ev.PaymentStatus != "paid" || ev.TestMode {
		t.Fatalf("payment event = %+v", ev)
	}

	ev, err = parseWebhook([]byte(`{"meta":{"event_name":"order_created"},"data":{"type":"orders","id":"5","attributes":{"test_mode":true}}}`))
	if err != nil || ev.Kind != billing.EventOther || !ev.TestMode {
		t.Fatalf("other event = %+v %v", ev, err)
	}
	if _, err := parseWebhook([]byte(`{"meta":{}}`)); err == nil {
		t.Fatal("event without a name accepted")
	}
}
