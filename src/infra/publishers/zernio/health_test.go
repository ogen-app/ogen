package zernio

import (
	"net/http"
	"testing"
	"time"
)

// TestGetAccountsHealth proves the bulk health call forwards the profileId
// filter, decodes the forward-looking token expiry + status/needsReconnect, and
// tolerates a missing tokenExpiresAt (nil) on an already-broken account.
func TestGetAccountsHealth(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawProfile string
	stub.handle("GET", "/accounts/health", func(w http.ResponseWriter, r *http.Request) {
		sawProfile = r.URL.Query().Get("profileId")
		jsonResponse(http.StatusOK, map[string]any{
			"summary": map[string]any{"total": 2, "healthy": 1, "error": 1, "needsReconnect": 1},
			"accounts": []map[string]any{
				{
					"accountId":      "acc_ig",
					"profileId":      "p_test",
					"platform":       "instagram",
					"username":       "myaccount",
					"status":         "healthy",
					"tokenValid":     true,
					"tokenExpiresAt": "2026-09-15T00:00:00Z",
					"needsReconnect": false,
					"canPost":        true,
					"issues":         []string{},
				},
				{
					"accountId":      "acc_x",
					"profileId":      "p_test",
					"platform":       "twitter",
					"username":       "mytwitter",
					"status":         "error",
					"tokenValid":     false,
					"needsReconnect": true,
					"canPost":        false,
					"issues":         []string{"Token expired"},
				},
			},
		})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	got, err := c.GetAccountsHealth(t.Context(), "p_test")
	if err != nil {
		t.Fatalf("GetAccountsHealth: %v", err)
	}
	if sawProfile != "p_test" {
		t.Errorf("profileId filter: got %q want p_test", sawProfile)
	}
	if len(got) != 2 {
		t.Fatalf("accounts: got %d want 2", len(got))
	}

	healthy := got[0]
	if healthy.AccountID != "acc_ig" || healthy.Status != "healthy" || !healthy.TokenValid {
		t.Errorf("healthy account decoded wrong: %+v", healthy)
	}
	if healthy.TokenExpiresAt == nil {
		t.Fatal("healthy account: tokenExpiresAt should be set")
	}
	if want := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC); !healthy.TokenExpiresAt.Equal(want) {
		t.Errorf("tokenExpiresAt: got %v want %v", healthy.TokenExpiresAt, want)
	}

	broken := got[1]
	if broken.AccountID != "acc_x" || broken.Status != "error" || !broken.NeedsReconnect {
		t.Errorf("broken account decoded wrong: %+v", broken)
	}
	if broken.TokenExpiresAt != nil {
		t.Errorf("broken account: tokenExpiresAt should be nil, got %v", broken.TokenExpiresAt)
	}
}

// TestGetAccountsHealthFiltersForeignProfile proves the client-side profile
// filter drops an account Zernio echoes for a different profile.
func TestGetAccountsHealthFiltersForeignProfile(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	stub.handle("GET", "/accounts/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(http.StatusOK, map[string]any{
			"accounts": []map[string]any{
				{"accountId": "mine", "profileId": "p_test", "platform": "linkedin", "status": "warning"},
				{"accountId": "theirs", "profileId": "p_other", "platform": "linkedin", "status": "healthy"},
			},
		})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	got, err := c.GetAccountsHealth(t.Context(), "p_test")
	if err != nil {
		t.Fatalf("GetAccountsHealth: %v", err)
	}
	if len(got) != 1 || got[0].AccountID != "mine" {
		t.Fatalf("expected only p_test's account, got %+v", got)
	}
}
