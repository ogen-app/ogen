package anthropicadmin

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sk-ant-admin01-test"

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(func(context.Context) (string, error) { return testKey, nil }, srv.URL, 5*time.Second)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, &slept
}

func serveFile(t *testing.T, w http.ResponseWriter, name string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func TestDailyCostsPagesAndAggregates(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/organizations/cost_report" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != testKey {
			t.Errorf("x-api-key = %q", got)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header missing")
		}
		q := r.URL.Query()
		if q.Get("starting_at") != "2026-10-01T00:00:00Z" || q.Get("ending_at") != "2026-10-04T00:00:00Z" {
			t.Errorf("range = %s .. %s", q.Get("starting_at"), q.Get("ending_at"))
		}
		if q.Get("bucket_width") != "1d" || q.Get("group_by[]") != "description" || q.Get("limit") != "31" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		switch q.Get("page") {
		case "":
			serveFile(t, w, "cost_report_page1.json")
		case "page_MjAyNi0xMC0wMlQwMDowMDowMFo=":
			serveFile(t, w, "cost_report_page2.json")
		default:
			t.Errorf("unexpected page %q", q.Get("page"))
		}
	})

	got, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-03"))
	if err != nil {
		t.Fatal(err)
	}
	want := []DailyCost{
		// The out-of-range Sep 30 bucket is ignored.
		{Date: "2026-10-01", Model: "", CostType: "web_search", USDMicros: 1_000_000},
		{Date: "2026-10-01", Model: "claude-haiku-5-5", CostType: "tokens", USDMicros: 155_000},
		// 1234.56789 + 200.00001 cents, summed exactly across service tiers.
		{Date: "2026-10-01", Model: "claude-sonnet-5", CostType: "tokens", USDMicros: 14_345_679},
		// 0.00004 cents = 0.4 micros rounds to zero and is dropped; 0.5 rounds up.
		{Date: "2026-10-03", Model: "claude-sonnet-5", CostType: "tokens", USDMicros: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("costs =\n%+v\nwant\n%+v", got, want)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestDailyCostsNotConfigured(t *testing.T) {
	c := New(func(context.Context) (string, error) { return "", nil }, "http://127.0.0.1:1", time.Second)
	if _, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestDailyCostsUnauthorizedIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, http.StatusUnauthorized)
	})
	if _, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestDailyCostsRetries429And5xx(t *testing.T) {
	var calls atomic.Int32
	c, slept := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, http.StatusTooManyRequests)
		case 2:
			http.Error(w, "upstream", http.StatusBadGateway)
		default:
			serveFile(t, w, "cost_report_page2.json")
		}
	})
	got, err := c.DailyCosts(t.Context(), day("2026-10-02"), day("2026-10-03"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].USDMicros != 1 {
		t.Fatalf("costs = %+v", got)
	}
	if want := []time.Duration{7 * time.Second, 2 * time.Second}; !slices.Equal(*slept, want) {
		t.Fatalf("backoffs = %v, want %v", *slept, want)
	}
}

func TestDailyCostsUnavailableAfterRetries(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529)
	})
	_, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls.Load() != maxAttempts {
		t.Fatalf("calls = %d, want %d", calls.Load(), maxAttempts)
	}
}

func TestDailyCostsRejectsNonUSD(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"starting_at":"2026-10-01T00:00:00Z","results":[{"amount":"1","currency":"EUR"}]}],"has_more":false}`))
	})
	if _, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01")); err == nil {
		t.Fatal("want an error for a non-USD amount")
	}
}

func TestCentsToMicrosRounding(t *testing.T) {
	for _, tc := range []struct {
		cents string
		want  int64
	}{
		{"0", 0},
		{"1", 10_000},
		{"123.45", 1_234_500},
		{"0.00005", 1},
		{"0.000049", 0},
		{"-0.00005", -1},
		{"99999999.99999", 1_000_000_000_000},
	} {
		r, _ := new(big.Rat).SetString(tc.cents)
		if got, err := centsToMicros(r); err != nil || got != tc.want {
			t.Errorf("centsToMicros(%s) = %d, %v, want %d", tc.cents, got, err, tc.want)
		}
	}
}

func TestDailyCostsRejectsOverflow(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// 10^15 cents is 10^19 micros, past int64.
		_, _ = w.Write([]byte(`{"data":[{"starting_at":"2026-10-01T00:00:00Z","results":[{"amount":"1000000000000000","currency":"USD"}]}],"has_more":false}`))
	})
	if _, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01")); err == nil {
		t.Fatal("want an error for an amount past int64 micros")
	}
}

func TestDailyCostsForbiddenIsDistinct(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"type":"error","error":{"type":"permission_error","message":"not allowed"}}`, http.StatusForbidden)
	})
	_, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01"))
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrForbidden only", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestDailyCostsLongRetryAfterGivesUp(t *testing.T) {
	var calls atomic.Int32
	c, slept := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		http.Error(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, http.StatusTooManyRequests)
	})
	_, err := c.DailyCosts(t.Context(), day("2026-10-01"), day("2026-10-01"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls.Load() != 1 || len(*slept) != 0 {
		t.Fatalf("calls = %d, waits = %v; want one call and no wait", calls.Load(), *slept)
	}
}

func TestSleepCtxHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
