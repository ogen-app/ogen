package zernio

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sampleAnalyticsItem is the documented per-post response object shape
// (CON-93 §2), returned inside the list envelope's `analytics` array.
const sampleAnalyticsItem = `{
  "postId": "665f-abc",
  "latePostId": "late-665f",
  "status": "published",
  "content": "hello world",
  "publishedAt": "2026-06-15T09:00:00Z",
  "platform": "linkedin",
  "isExternal": false,
  "syncStatus": "synced",
  "analytics": {
    "impressions": 1234, "reach": 1000, "likes": 56, "comments": 7,
    "shares": 3, "saves": 5, "clicks": 12, "views": 800,
    "engagementRate": 0.07, "lastUpdated": "2026-06-15T09:00:00Z"
  },
  "platformAnalytics": [
    {
      "platform": "linkedin", "status": "published",
      "platformPostId": "urn:li:share:1", "accountId": "acc-1",
      "accountUsername": "ogen", "platformPostUrl": "https://li/1",
      "syncStatus": "synced",
      "analytics": { "impressions": 1234, "likes": 56 }
    },
    {
      "platform": "instagram", "status": "published",
      "syncStatus": "unavailable",
      "errorMessage": "analytics scope not granted",
      "reauthorizeUrl": "https://zernio.com/reauth/ig",
      "analytics": {}
    }
  ]
}`

func TestListAnalyticsHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("GET", "/analytics", func(w http.ResponseWriter, r *http.Request) {
		// Documented params are forwarded verbatim.
		q := r.URL.Query()
		if q.Get("source") != "late" {
			t.Errorf("source: got %q want late", q.Get("source"))
		}
		if q.Get("limit") != "100" {
			t.Errorf("limit: got %q want 100", q.Get("limit"))
		}
		if q.Get("page") != "2" {
			t.Errorf("page: got %q want 2", q.Get("page"))
		}
		if q.Get("fromDate") != "2026-03-01" {
			t.Errorf("fromDate: got %q want 2026-03-01", q.Get("fromDate"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("auth header: got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"analytics":[` + sampleAnalyticsItem + `],"pagination":{"page":2,"limit":100,"total":150,"pages":2}}`))
	})

	c := newClient(s)
	items, page, err := c.ListAnalytics(t.Context(), AnalyticsQuery{
		Source:   AnalyticsSourceLate,
		FromDate: "2026-03-01",
		Limit:    100,
		Page:     2,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items: got %d want 1", len(items))
	}
	it := items[0]
	if it.PostID != "665f-abc" || it.LatePostID != "late-665f" {
		t.Errorf("ids: got postId=%q latePostId=%q", it.PostID, it.LatePostID)
	}
	if it.Analytics.Impressions != 1234 || it.Analytics.EngagementRate != 0.07 {
		t.Errorf("metrics: got impressions=%d rate=%v", it.Analytics.Impressions, it.Analytics.EngagementRate)
	}
	if it.Analytics.LastUpdated == nil {
		t.Errorf("lastUpdated should decode")
	}
	if len(it.PlatformAnalytics) != 2 {
		t.Fatalf("platformAnalytics: got %d want 2", len(it.PlatformAnalytics))
	}
	if it.PlatformAnalytics[1].SyncStatus != "unavailable" || it.PlatformAnalytics[1].ErrorMessage == "" {
		t.Errorf("per-platform scope gap not surfaced: %+v", it.PlatformAnalytics[1])
	}
	if len(it.Raw) == 0 {
		t.Errorf("Raw should retain the verbatim item")
	}
	if page.Total != 150 || page.Pages != 2 {
		t.Errorf("pagination: got %+v", page)
	}
}

// TestListAnalyticsTolerantShapes covers the top-level response shapes Zernio
// has shipped or documented for GET /analytics. Every shape must decode to the
// same one item — the rigid `{"analytics":[...]}`-only decoder silently
// collected nothing when the endpoint returned a bare array or a differently
// keyed envelope (CON-93 follow-up).
func TestListAnalyticsTolerantShapes(t *testing.T) {
	item := `{"postId":"p1","latePostId":"late-1","analytics":{"impressions":42,"likes":3,"follows":9}}`
	cases := []struct {
		name      string
		body      string
		wantPages int
	}{
		{"bare array", `[` + item + `]`, 0},
		{"analytics envelope", `{"analytics":[` + item + `],"pagination":{"pages":1}}`, 1},
		{"posts wrapper key", `{"posts":[` + item + `],"pagination":{"totalPages":2}}`, 2},
		{"data wrapper key", `{"data":[` + item + `]}`, 0},
		{"results wrapper key", `{"results":[` + item + `]}`, 0},
		{"single post object", item, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub()
			defer s.Close()
			s.handle("GET", "/analytics", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			})

			c := newClient(s)
			items, page, err := c.ListAnalytics(t.Context(), AnalyticsQuery{Source: AnalyticsSourceLate})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("items: got %d want 1 (shape %q)", len(items), tc.name)
			}
			it := items[0]
			if it.PostID != "p1" || it.LatePostID != "late-1" {
				t.Errorf("ids: got postId=%q latePostId=%q", it.PostID, it.LatePostID)
			}
			if it.Analytics.Impressions != 42 || it.Analytics.Follows != 9 {
				t.Errorf("metrics: got impressions=%d follows=%d", it.Analytics.Impressions, it.Analytics.Follows)
			}
			if len(it.Raw) == 0 {
				t.Errorf("Raw should retain the verbatim item")
			}
			if page.LastPage() != tc.wantPages {
				t.Errorf("LastPage: got %d want %d", page.LastPage(), tc.wantPages)
			}
		})
	}
}

// TestListAnalyticsTolerantTimestamps guards the production incident where
// Zernio returned a space-separated, zone-less `lastUpdated`
// ("2026-07-29 12:47:25") that Go's stdlib time.Time.UnmarshalJSON rejects.
// The old rigid *time.Time typing failed the whole item — and one bad item
// fails the whole page — so a single such timestamp zeroed out that tenant's
// entire analytics collection (the observed last_refresh_status error). Every
// accepted shape must decode with metrics intact; an unparseable value must
// degrade to a nil timestamp rather than erroring the page.
func TestListAnalyticsTolerantTimestamps(t *testing.T) {
	cases := []struct {
		name        string
		lastUpdated string // raw JSON value as it appears on the wire
		want        *time.Time
	}{
		{"space separated zoneless (the incident)", `"2026-07-29 12:47:25"`, timePtr(time.Date(2026, 7, 29, 12, 47, 25, 0, time.UTC))},
		{"rfc3339 still works", `"2026-06-15T09:00:00Z"`, timePtr(time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))},
		{"date only", `"2026-07-29"`, timePtr(time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC))},
		{"unparseable degrades to nil", `"whenever"`, nil},
		{"null degrades to nil", `null`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"analytics":[{"postId":"p1","latePostId":"late-1","analytics":` +
				`{"impressions":1234,"likes":56,"lastUpdated":` + tc.lastUpdated + `}}]}`
			s := newStub()
			defer s.Close()
			s.handle("GET", "/analytics", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})

			c := newClient(s)
			items, _, err := c.ListAnalytics(t.Context(), AnalyticsQuery{Source: AnalyticsSourceLate})
			if err != nil {
				t.Fatalf("decode must tolerate lastUpdated %s, got error: %v", tc.lastUpdated, err)
			}
			if len(items) != 1 {
				t.Fatalf("items: got %d want 1 (a bad timestamp must not drop the item)", len(items))
			}
			// Metrics always land, whatever the timestamp shape.
			if items[0].Analytics.Impressions != 1234 || items[0].Analytics.Likes != 56 {
				t.Errorf("metrics dropped: %+v", items[0].Analytics)
			}
			got := items[0].Analytics.LastUpdatedTime()
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("lastUpdated: got %v want nil", got)
			case tc.want != nil && (got == nil || !got.Equal(*tc.want)):
				t.Errorf("lastUpdated: got %v want %v", got, tc.want)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestListAnalyticsUnknownEnvelopeIsEmptyNotError ensures an object with no
// recognizable array key and no post id decodes to an empty page rather than a
// hard error, so an unexpected wrapper degrades to a no-op tick.
func TestListAnalyticsUnknownEnvelopeIsEmptyNotError(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/analytics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"unexpected":{"nested":true},"pagination":{"pages":1}}`))
	})
	c := newClient(s)
	items, _, err := c.ListAnalytics(t.Context(), AnalyticsQuery{Source: AnalyticsSourceLate})
	if err != nil {
		t.Fatalf("unknown envelope should not error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items: got %d want 0", len(items))
	}
}

func TestListAnalyticsLegacy402MapsToSentinel(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/analytics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "analytics add-on required"})
	})

	c := newClient(s)
	_, _, err := c.ListAnalytics(t.Context(), AnalyticsQuery{Source: AnalyticsSourceLate})
	if !errors.Is(err, ErrAnalyticsUnavailable) {
		t.Fatalf("err: got %v want ErrAnalyticsUnavailable", err)
	}
}

func TestListAnalyticsPropagatesStatusErrors(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/analytics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow down"})
	})

	c := newClient(s)
	_, _, err := c.ListAnalytics(t.Context(), AnalyticsQuery{Source: AnalyticsSourceLate})
	if !IsStatus(err, http.StatusTooManyRequests) {
		t.Fatalf("err: got %v want 429 APIError", err)
	}
	// The bearer key must never leak into the error.
	if strings.Contains(err.Error(), "key") {
		t.Errorf("error leaked credential material: %v", err)
	}
}

func TestListAnalyticsNilClientDisabled(t *testing.T) {
	var c *Client
	_, _, err := c.ListAnalytics(t.Context(), AnalyticsQuery{})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("nil client should be disabled, got %v", err)
	}
}
