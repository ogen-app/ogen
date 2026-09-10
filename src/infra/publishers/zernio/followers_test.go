package zernio

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestGetFollowerStatsHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("GET", "/accounts/follower-stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("accountIds") != "acc-1,acc-2" {
			t.Errorf("accountIds: got %q want acc-1,acc-2", q.Get("accountIds"))
		}
		if q.Get("profileId") != "prof-1" {
			t.Errorf("profileId: got %q want prof-1", q.Get("profileId"))
		}
		if q.Get("granularity") != "daily" {
			t.Errorf("granularity: got %q want daily", q.Get("granularity"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": [
				{"_id":"acc-1","platform":"instagram","username":"acme","currentFollowers":10432,"growth":128,"growthPercentage":1.24,"dataPoints":30},
				{"_id":"acc-2","platform":"tiktok","username":"acme","currentFollowers":501,"growth":-3,"growthPercentage":-0.6,"dataPoints":30}
			],
			"stats": {
				"acc-1": [{"date":"2026-07-01","followers":10304},{"date":"2026-07-02","followers":10360}],
				"acc-2": [{"date":"2026-07-01","followers":504}]
			},
			"dateRange": {"from":"2026-07-01T00:00:00Z","to":"2026-07-30T00:00:00Z"},
			"granularity": "daily"
		}`))
	})

	c := newClient(s)
	out, err := c.GetFollowerStats(t.Context(), FollowerQuery{
		AccountIDs:  []string{"acc-1", "acc-2"},
		ProfileID:   "prof-1",
		Granularity: "daily",
	})
	if err != nil {
		t.Fatalf("follower-stats: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("accounts: got %d want 2", len(out.Accounts))
	}
	if out.Accounts[0].ID != "acc-1" || out.Accounts[0].CurrentFollowers != 10432 || out.Accounts[0].GrowthPercentage != 1.24 {
		t.Errorf("account[0]: %+v", out.Accounts[0])
	}
	if out.Accounts[1].Growth != -3 {
		t.Errorf("negative growth should decode: %+v", out.Accounts[1])
	}
	if len(out.Stats["acc-1"]) != 2 || out.Stats["acc-1"][1].Followers != 10360 {
		t.Errorf("series acc-1: %+v", out.Stats["acc-1"])
	}
	if out.Granularity != "daily" {
		t.Errorf("granularity: got %q", out.Granularity)
	}
}

func TestGetFollowerStatsAddonRequired(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/accounts/follower-stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Analytics add-on required","requiresAddon":true}`))
	})
	c := newClient(s)
	_, err := c.GetFollowerStats(t.Context(), FollowerQuery{ProfileID: "p"})
	if !errors.Is(err, ErrAnalyticsUnavailable) {
		t.Fatalf("want ErrAnalyticsUnavailable, got %v", err)
	}
}

func TestSyncExternalPostHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("POST", "/posts/sync-external", func(w http.ResponseWriter, r *http.Request) {
		var req SyncExternalRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.AccountID != "acc-1" {
			t.Errorf("accountId: got %q want acc-1", req.AccountID)
		}
		if req.URL != "https://instagram.com/p/abc" {
			t.Errorf("url: got %q", req.URL)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"synced": map[string]any{"postsFound": 4, "postsSynced": 1, "skipped": false},
			"found":  true,
			"post": map[string]any{
				"platform":        "instagram",
				"platformPostId":  "IG-123",
				"platformPostUrl": "https://instagram.com/p/abc",
				"content":         "hello",
				"publishedAt":     "2026-07-30 12:47:25",
				"mediaType":       "image",
				"analytics": map[string]any{
					"likes": 12, "comments": 3, "reach": 0, "impressions": 0,
					"engagementRate": 0.0, "lastUpdated": "2026-07-30T13:00:00Z",
				},
			},
		})
	})

	c := newClient(s)
	out, err := c.SyncExternalPost(t.Context(), SyncExternalRequest{
		AccountID: "acc-1",
		URL:       "https://instagram.com/p/abc",
	})
	if err != nil {
		t.Fatalf("sync-external: %v", err)
	}
	if !out.Found || out.Post == nil {
		t.Fatalf("expected found post, got %+v", out)
	}
	if out.Post.PlatformPostID != "IG-123" || out.Post.Analytics.Likes != 12 {
		t.Errorf("post: %+v", out.Post)
	}
	// Zoneless publishedAt must decode via flexTime.
	if out.Post.PublishedAtTime() == nil {
		t.Errorf("publishedAt should decode via flexTime")
	}
	if out.Synced.PostsSynced != 1 {
		t.Errorf("synced summary: %+v", out.Synced)
	}
	if len(out.Raw) == 0 {
		t.Errorf("Raw should retain the verbatim body")
	}
}

func TestSyncExternalPostRequiresAccountID(t *testing.T) {
	c := NewClient(StaticKey("key"), "http://unused", ClientOpts{})
	_, err := c.SyncExternalPost(t.Context(), SyncExternalRequest{URL: "https://x/y"})
	if err == nil {
		t.Fatalf("expected error when accountId is empty")
	}
}

func TestSyncExternalPostNotFound(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("POST", "/posts/sync-external", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Post not found","type":"not_found"}`))
	})
	c := newClient(s)
	_, err := c.SyncExternalPost(t.Context(), SyncExternalRequest{AccountID: "acc-1", URL: "https://x/y"})
	if !IsStatus(err, http.StatusNotFound) {
		t.Fatalf("want *APIError{404}, got %v", err)
	}
}
