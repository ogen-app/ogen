package zernio

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestCreateConnectLinkIsHeadless proves CON-217 switched connect-link issuance
// to headless mode and forwards the per-call redirect URL.
func TestCreateConnectLinkIsHeadless(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawHeadless, sawRedirect string
	stub.handle("GET", "/connect/linkedin", func(w http.ResponseWriter, r *http.Request) {
		sawHeadless = r.URL.Query().Get("headless")
		sawRedirect = r.URL.Query().Get("redirect_url")
		jsonResponse(http.StatusOK, map[string]string{"authUrl": "https://zernio.com/oauth/linkedin?token=t"})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	cb := "https://app.example.com/api/integrations/zernio/connect/callback?ogen_cn=abc"
	if _, err := c.CreateConnectLink(t.Context(), "p_test", "linkedin", cb); err != nil {
		t.Fatalf("CreateConnectLink: %v", err)
	}
	if sawHeadless != "true" {
		t.Errorf("headless flag: got %q want true", sawHeadless)
	}
	if sawRedirect != cb {
		t.Errorf("redirect_url: got %q want %q", sawRedirect, cb)
	}
}

// TestListConnectTargets proves the list step authenticates with the
// X-Connect-Token header and tolerantly decodes the `pages` envelope.
func TestListConnectTargets(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawToken, sawTemp, sawProfile string
	stub.handle("GET", "/connect/facebook/select-page", func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Connect-Token")
		sawTemp = r.URL.Query().Get("tempToken")
		sawProfile = r.URL.Query().Get("profileId")
		jsonResponse(http.StatusOK, map[string]any{
			"pages": []map[string]string{
				{"id": "111", "name": "Acme Corp", "username": "acme"},
				{"id": "222", "name": "Acme EU"},
			},
		})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	targets, err := c.ListConnectTargets(t.Context(), "facebook", "p_test", "temp-xyz", "ct-secret")
	if err != nil {
		t.Fatalf("ListConnectTargets: %v", err)
	}
	if sawToken != "ct-secret" {
		t.Errorf("X-Connect-Token: got %q want ct-secret", sawToken)
	}
	if sawTemp != "temp-xyz" || sawProfile != "p_test" {
		t.Errorf("query: tempToken=%q profileId=%q", sawTemp, sawProfile)
	}
	if len(targets) != 2 {
		t.Fatalf("targets: got %d want 2", len(targets))
	}
	if targets[0].ID != "111" || targets[0].Name != "Acme Corp" || targets[0].Kind != "page" {
		t.Errorf("target[0] decoded wrong: %+v", targets[0])
	}
}

// TestSelectConnectTarget proves the finalize step posts the platform's id
// field + tempToken under the X-Connect-Token header.
func TestSelectConnectTarget(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var body map[string]any
	var sawToken string
	stub.handle("POST", "/connect/facebook/select-page", func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Connect-Token")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		jsonResponse(http.StatusOK, map[string]string{"redirect_url": "https://app.example.com/done?connected=facebook"})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	up := json.RawMessage(`{"id":"u1"}`)
	if err := c.SelectConnectTarget(t.Context(), "facebook", "p_test", "temp-xyz", "ct-secret", "111", up); err != nil {
		t.Fatalf("SelectConnectTarget: %v", err)
	}
	if sawToken != "ct-secret" {
		t.Errorf("X-Connect-Token: got %q want ct-secret", sawToken)
	}
	if body["pageId"] != "111" {
		t.Errorf("pageId: got %v want 111", body["pageId"])
	}
	if body["profileId"] != "p_test" || body["tempToken"] != "temp-xyz" {
		t.Errorf("body: profileId=%v tempToken=%v", body["profileId"], body["tempToken"])
	}
}
