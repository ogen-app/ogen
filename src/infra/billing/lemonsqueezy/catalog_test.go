package lemonsqueezy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// catalogServer answers the store and two pages of products, recording the
// key every request used.
func catalogServer(t *testing.T) (string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		switch {
		case r.URL.Path == "/stores/77":
			_, _ = io.WriteString(w, `{"data":{"type":"stores","id":"77","attributes":{"currency":"EUR"}}}`)
		case r.URL.Path == "/products" && r.URL.Query().Get("filter[store_id]") == "77" && r.URL.Query().Get("include") == "variants":
			switch r.URL.Query().Get("page[number]") {
			case "1":
				_, _ = io.WriteString(w, `{"meta":{"page":{"lastPage":2}},
					"data":[{"type":"products","id":"10","attributes":{"name":"Pro"}}],
					"included":[
						{"type":"variants","id":"100","attributes":{"product_id":10,"name":"Default","price":0,"is_subscription":false,"status":"pending","test_mode":true}},
						{"type":"variants","id":"101","attributes":{"product_id":10,"name":"Monthly","price":5000,"is_subscription":true,"interval":"month","interval_count":1,"has_free_trial":true,"trial_interval":"month","trial_interval_count":1,"status":"published","test_mode":true}},
						{"type":"variants","id":"102","attributes":{"product_id":10,"name":"Yearly","price":50000,"is_subscription":true,"interval":"year","interval_count":1,"status":"published","test_mode":true}}
					]}`)
			case "2":
				_, _ = io.WriteString(w, `{"meta":{"page":{"lastPage":2}},
					"data":[{"type":"products","id":"20","attributes":{"name":"E-book"}}],
					"included":[{"type":"variants","id":"201","attributes":{"product_id":20,"name":"Default","price":900,"is_subscription":false,"status":"published","test_mode":true}}]}`)
			default:
				http.Error(w, "bad page", http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &keys
}

func TestCatalog(t *testing.T) {
	base, keys := catalogServer(t)
	c := New(Options{APIKey: static("test-key"), StoreID: "77", TestMode: true, BaseURL: base, Timeout: time.Second})
	cat, err := c.Catalog(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Currency != "EUR" || !cat.TestMode || len(cat.Variants) != 2 {
		t.Fatalf("catalog = %+v", cat)
	}
	m, ok := cat.Variant("101")
	if !ok || m.ProductName != "Pro" || m.Name != "Monthly" || m.PriceMinor != 5000 || m.Interval != "month" || m.IntervalCount != 1 || !m.OneMonthTrial() {
		t.Fatalf("monthly = %+v", m)
	}
	if _, ok := cat.Variant("201"); ok {
		t.Fatal("one-off variant listed")
	}
	if _, ok := cat.Variant("100"); ok {
		t.Fatal("pending default variant listed")
	}
	if len(*keys) != 3 {
		t.Fatalf("requests = %d, want store + 2 pages", len(*keys))
	}
}

func TestCatalogModeKeys(t *testing.T) {
	base, keys := catalogServer(t)

	// In test mode, live reads use the live key; without one they are not
	// configured.
	c := New(Options{APIKey: static("test-key"), LiveAPIKey: static("live-key"), StoreID: "77", TestMode: true, BaseURL: base})
	if _, err := c.Catalog(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if got := (*keys)[0]; got != "Bearer live-key" {
		t.Fatalf("live read used %q", got)
	}
	noLive := New(Options{APIKey: static("test-key"), StoreID: "77", TestMode: true, BaseURL: base})
	if _, err := noLive.Catalog(t.Context(), false); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("live read without a live key: %v", err)
	}
	unsetLive := New(Options{APIKey: static("test-key"), LiveAPIKey: static(""), StoreID: "77", TestMode: true, BaseURL: base})
	if _, err := unsetLive.Catalog(t.Context(), false); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("live read with an unset live key: %v", err)
	}

	// Once live, the server's own key reads live mode and test mode is gone.
	*keys = nil
	live := New(Options{APIKey: static("live-key"), StoreID: "77", BaseURL: base})
	if _, err := live.Catalog(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if got := (*keys)[0]; got != "Bearer live-key" {
		t.Fatalf("live server read used %q", got)
	}
	if _, err := live.Catalog(t.Context(), true); !errors.Is(err, billing.ErrNotConfigured) {
		t.Fatalf("test read on a live server: %v", err)
	}
}
