package zernio

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

// TestCreateConnectLinkIsHeadless proves CON-217 switched connect-link issuance
// to headless mode and forwards the per-call redirect URL.
func TestCreateConnectLinkIsHeadless(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawHeadless, sawRedirect string
	stub.handle("GET", "/connect/facebook", func(w http.ResponseWriter, r *http.Request) {
		sawHeadless = r.URL.Query().Get("headless")
		sawRedirect = r.URL.Query().Get("redirect_url")
		jsonResponse(http.StatusOK, map[string]string{"authUrl": "https://zernio.com/oauth/facebook?token=t"})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	cb := "https://app.example.com/api/integrations/zernio/connect/callback?ogen_cn=abc"
	if _, err := c.CreateConnectLink(t.Context(), "p_test", "facebook", cb); err != nil {
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
	sel, err := c.ListConnectTargets(t.Context(), "facebook", "p_test", ConnectCallback{TempToken: "temp-xyz", ConnectToken: "ct-secret"})
	if err != nil {
		t.Fatalf("ListConnectTargets: %v", err)
	}
	targets := sel.Targets
	if sel.TempToken != "temp-xyz" {
		t.Errorf("TempToken: got %q want temp-xyz", sel.TempToken)
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
	if err := c.SelectConnectTarget(t.Context(), "facebook", "p_test", "temp-xyz", "ct-secret", ConnectTarget{ID: "111", Kind: "page"}, up); err != nil {
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

// TestCreateConnectLinkLinkedInPendingDataOnly proves LinkedIn's redirect asks
// Zernio for a pendingDataToken instead of the OAuth token and org list, and
// keeps our own callback params.
func TestCreateConnectLinkLinkedInPendingDataOnly(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawRedirect string
	for _, p := range []string{"linkedin", "facebook"} {
		stub.handle("GET", "/connect/"+p, func(w http.ResponseWriter, r *http.Request) {
			sawRedirect = r.URL.Query().Get("redirect_url")
			jsonResponse(http.StatusOK, map[string]string{"authUrl": "https://zernio.com/oauth"})(w, r)
		})
	}

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	cb := "https://app.example.com/api/integrations/zernio/connect/callback?ogen_cn=abc"
	if _, err := c.CreateConnectLink(t.Context(), "p_test", "linkedin", cb); err != nil {
		t.Fatalf("CreateConnectLink: %v", err)
	}
	u, err := url.Parse(sawRedirect)
	if err != nil {
		t.Fatalf("redirect_url %q: %v", sawRedirect, err)
	}
	if u.Query().Get("pendingDataOnly") != "true" || u.Query().Get("ogen_cn") != "abc" {
		t.Errorf("linkedin redirect_url: got %q, want ogen_cn=abc and pendingDataOnly=true", sawRedirect)
	}

	if _, err := c.CreateConnectLink(t.Context(), "p_test", "facebook", cb); err != nil {
		t.Fatalf("CreateConnectLink: %v", err)
	}
	if sawRedirect != cb {
		t.Errorf("facebook redirect_url: got %q want %q", sawRedirect, cb)
	}
}

// TestListConnectTargetsLinkedInPendingData proves LinkedIn's targets come from
// the pending-data read — never from a GET on select-organization, which
// Zernio answers with 405 — and that the member's own profile leads the list.
func TestListConnectTargetsLinkedInPendingData(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawToken string
	stub.handle("GET", "/connect/pending-data", func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.URL.Query().Get("token")
		jsonResponse(http.StatusOK, map[string]any{
			"platform":    "linkedin",
			"tempToken":   "AQV-temp",
			"userProfile": map[string]string{"id": "m1", "displayName": "Jane Doe", "profilePicture": "https://img/jane"},
			"organizations": []map[string]string{
				{"id": "12345", "urn": "urn:li:organization:12345", "name": "Acme Corp", "vanityName": "acme-corp"},
				{"id": "urn:li:organization:67890", "name": "Example Inc"},
			},
		})(w, r)
	})
	stub.handle("GET", "/connect/linkedin/select-organization", func(w http.ResponseWriter, r *http.Request) {
		t.Error("listed LinkedIn targets with a GET on select-organization")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	sel, err := c.ListConnectTargets(t.Context(), "linkedin", "p_test", ConnectCallback{ConnectToken: "ct", PendingDataToken: "pdt-1"})
	if err != nil {
		t.Fatalf("ListConnectTargets: %v", err)
	}
	if sawToken != "pdt-1" {
		t.Errorf("pending-data token: got %q want pdt-1", sawToken)
	}
	if sel.TempToken != "AQV-temp" {
		t.Errorf("TempToken: got %q want AQV-temp", sel.TempToken)
	}
	var up map[string]string
	if err := json.Unmarshal(sel.UserProfile, &up); err != nil || up["id"] != "m1" {
		t.Errorf("UserProfile: got %s (%v)", sel.UserProfile, err)
	}
	want := []ConnectTarget{
		{ID: "personal", Name: "Jane Doe", Kind: KindPersonal, AvatarURL: "https://img/jane"},
		{ID: "12345", Name: "Acme Corp", Username: "acme-corp", Kind: KindOrganization},
		{ID: "67890", Name: "Example Inc", Kind: KindOrganization},
	}
	if !slices.Equal(sel.Targets, want) {
		t.Errorf("targets:\n got %+v\nwant %+v", sel.Targets, want)
	}
}

// TestListConnectTargetsLinkedInInline proves a redirect that carried the
// token, profile and organizations itself needs no Zernio call.
func TestListConnectTargetsLinkedInInline(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	sel, err := c.ListConnectTargets(t.Context(), "linkedin", "p_test", ConnectCallback{
		TempToken:   "temp",
		UserProfile: json.RawMessage(`{"id":"m1","displayName":"Jane Doe"}`),
	})
	if err != nil {
		t.Fatalf("ListConnectTargets: %v", err)
	}
	if len(sel.Targets) != 1 || sel.Targets[0].Kind != KindPersonal || sel.TempToken != "temp" {
		t.Errorf("got %+v, want only the personal profile and the redirect's token", sel)
	}
}

// TestSelectConnectTargetLinkedIn proves LinkedIn's select posts accountType,
// with selectedOrganization {id, urn, name} for a company page.
func TestSelectConnectTargetLinkedIn(t *testing.T) {
	cases := []struct {
		name   string
		target ConnectTarget
		want   map[string]any
	}{
		{
			name:   "personal",
			target: ConnectTarget{ID: "personal", Name: "Jane Doe", Kind: KindPersonal},
			want:   map[string]any{"accountType": "personal"},
		},
		{
			name:   "organization",
			target: ConnectTarget{ID: "12345", Name: "Acme Corp", Username: "acme-corp", Kind: KindOrganization},
			want: map[string]any{
				"accountType": "organization",
				"selectedOrganization": map[string]any{
					"id": "12345", "urn": "urn:li:organization:12345", "name": "Acme Corp", "vanityName": "acme-corp",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubServer()
			defer stub.Close()
			var body map[string]any
			stub.handle("POST", "/connect/linkedin/select-organization", func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)
				jsonResponse(http.StatusOK, map[string]string{"message": "ok"})(w, r)
			})

			c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
			if err := c.SelectConnectTarget(t.Context(), "linkedin", "p_test", "temp", "ct", tc.target, json.RawMessage(`{"id":"m1"}`)); err != nil {
				t.Fatalf("SelectConnectTarget: %v", err)
			}
			if body["accountType"] != tc.want["accountType"] {
				t.Errorf("accountType: got %v want %v", body["accountType"], tc.want["accountType"])
			}
			if got, want := fmt.Sprint(body["selectedOrganization"]), fmt.Sprint(tc.want["selectedOrganization"]); got != want {
				t.Errorf("selectedOrganization: got %s want %s", got, want)
			}
			if _, ok := body["organizationId"]; ok {
				t.Error("body carries organizationId, which Zernio does not read")
			}
			if body["profileId"] != "p_test" || body["tempToken"] != "temp" || body["userProfile"] == nil {
				t.Errorf("body: %v", body)
			}
		})
	}
}
