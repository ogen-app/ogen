package zernio

import (
	"errors"
	"net/http"
	"testing"
)

func TestGetBestTimesHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("GET", "/analytics/best-time", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("profileId") != "prof-1" {
			t.Errorf("profileId: got %q want prof-1", q.Get("profileId"))
		}
		if q.Get("platform") != "instagram" {
			t.Errorf("platform: got %q want instagram", q.Get("platform"))
		}
		if q.Get("source") != "all" {
			t.Errorf("source: got %q want all", q.Get("source"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("auth header: got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slots":[
			{"day_of_week":2,"hour":18,"avg_engagement":510.3,"post_count":15},
			{"day_of_week":0,"hour":9,"avg_engagement":342.5,"post_count":12}
		]}`))
	})

	c := newClient(s)
	out, err := c.GetBestTimes(t.Context(), InsightQuery{
		ProfileID: "prof-1",
		Platform:  "instagram",
		Source:    AnalyticsSourceAll,
	})
	if err != nil {
		t.Fatalf("best-times: %v", err)
	}
	if len(out.Slots) != 2 {
		t.Fatalf("slots: got %d want 2", len(out.Slots))
	}
	if out.Slots[0].DayOfWeek != 2 || out.Slots[0].Hour != 18 || out.Slots[0].AvgEngagement != 510.3 || out.Slots[0].PostCount != 15 {
		t.Errorf("slot[0]: %+v", out.Slots[0])
	}
	if len(out.Raw) == 0 {
		t.Errorf("Raw should retain the verbatim body")
	}
}

func TestGetContentDecayHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("GET", "/analytics/content-decay", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"buckets":[
			{"bucket_order":0,"bucket_label":"0-6h","avg_pct_of_final":45.2,"post_count":89},
			{"bucket_order":1,"bucket_label":"6-12h","avg_pct_of_final":18.7,"post_count":89}
		]}`))
	})

	c := newClient(s)
	out, err := c.GetContentDecay(t.Context(), InsightQuery{ProfileID: "prof-1"})
	if err != nil {
		t.Fatalf("content-decay: %v", err)
	}
	if len(out.Buckets) != 2 {
		t.Fatalf("buckets: got %d want 2", len(out.Buckets))
	}
	if out.Buckets[0].BucketLabel != "0-6h" || out.Buckets[0].AvgPctOfFinal != 45.2 {
		t.Errorf("bucket[0]: %+v", out.Buckets[0])
	}
}

func TestGetPostingFrequencyHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("GET", "/analytics/posting-frequency", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"frequency":[
			{"platform":"instagram","posts_per_week":3,"avg_engagement_rate":4.1,"avg_engagement":812.0,"weeks_count":12}
		]}`))
	})

	c := newClient(s)
	out, err := c.GetPostingFrequency(t.Context(), InsightQuery{ProfileID: "prof-1"})
	if err != nil {
		t.Fatalf("posting-frequency: %v", err)
	}
	if len(out.Frequency) != 1 {
		t.Fatalf("frequency: got %d want 1", len(out.Frequency))
	}
	row := out.Frequency[0]
	if row.Platform != "instagram" || row.PostsPerWeek != 3 || row.AvgEngagementRate != 4.1 || row.WeeksCount != 12 {
		t.Errorf("row: %+v", row)
	}
}

// TestInsightsAddonGating verifies the shared add-on paywall mapping:
// both the legacy 402 and the current 403 become ErrAnalyticsUnavailable,
// while an unrelated status (401) stays a plain *APIError.
func TestInsightsAddonGating(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantSentin bool
	}{
		{"403 requiresAddon", http.StatusForbidden, `{"error":"Analytics add-on required","requiresAddon":true}`, true},
		{"403 without requiresAddon (proxy/WAF)", http.StatusForbidden, `{"error":"Forbidden"}`, false},
		{"legacy 402", http.StatusPaymentRequired, `{"error":"analytics add-on required"}`, true},
		{"401 unauthorized", http.StatusUnauthorized, `{"error":"Unauthorized"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub()
			defer s.Close()
			s.handle("GET", "/analytics/best-time", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := newClient(s)
			_, err := c.GetBestTimes(t.Context(), InsightQuery{ProfileID: "p"})
			if err == nil {
				t.Fatalf("expected error")
			}
			if got := errors.Is(err, ErrAnalyticsUnavailable); got != tc.wantSentin {
				t.Errorf("ErrAnalyticsUnavailable: got %v want %v (err=%v)", got, tc.wantSentin, err)
			}
			if !tc.wantSentin && !IsStatus(err, tc.status) {
				t.Errorf("expected *APIError{Status:%d}, got %v", tc.status, err)
			}
		})
	}
}

// TestInsightsEmptyBodyTolerant confirms a missing/empty array decodes to
// an empty result rather than erroring.
func TestInsightsEmptyBodyTolerant(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/analytics/best-time", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	c := newClient(s)
	out, err := c.GetBestTimes(t.Context(), InsightQuery{ProfileID: "p"})
	if err != nil {
		t.Fatalf("empty body should not error: %v", err)
	}
	if len(out.Slots) != 0 {
		t.Errorf("slots: got %d want 0", len(out.Slots))
	}
}
