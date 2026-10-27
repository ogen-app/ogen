package lemonsqueezy

import (
	"errors"
	"net/http"
	"testing"

	"github.com/ogen-app/ogen/src/domain/billing"
)

func TestPlanAndDecodeASubscriptionCall(t *testing.T) {
	call, err := Plan(func(c *Client) error {
		_, err := c.ChangePlan(t.Context(), "42", "7", billing.ChangeOpts{NoProration: true})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != http.MethodPatch || call.Path != "/subscriptions/42" || call.Headers["Content-Type"] != mediaType || len(call.Body) == 0 {
		t.Fatalf("call = %+v", call)
	}

	body := []byte(`{"data":{"type":"subscriptions","id":"42","attributes":{"status":"active","variant_id":7,"test_mode":true}}}`)
	var sub billing.Subscription
	err = Decode(Result{Status: 200, Body: body}, func(c *Client) error {
		var err error
		sub, err = c.GetSubscription(t.Context(), "42")
		return err
	})
	if err != nil || sub.ID != "42" || sub.VariantID != "7" {
		t.Fatalf("decoded %+v, %v", sub, err)
	}

	err = Decode(Result{Status: 404}, func(c *Client) error { _, err := c.GetSubscription(t.Context(), "42"); return err })
	if !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("404: %v, want ErrNotFound", err)
	}
	err = Decode(Result{Status: 503}, func(c *Client) error { _, err := c.GetSubscription(t.Context(), "42"); return err })
	if !IsRetryable(err) {
		t.Fatalf("503: %v, want retryable", err)
	}
	err = Decode(Result{Err: "connection reset"}, func(c *Client) error { _, err := c.GetSubscription(t.Context(), "42"); return err })
	if err == nil || !IsRetryable(err) {
		t.Fatalf("no response: %v, want a retryable error", err)
	}
}
