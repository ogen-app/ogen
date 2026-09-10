package zernio

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubServer captures the requests it sees and lets the test pre-load
// canned responses. Each entry in `routes` is matched by exact path +
// method; later additions to the same key override earlier ones.
type stubServer struct {
	*httptest.Server

	mu       sync.Mutex
	routes   map[string]http.HandlerFunc
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Auth   string
}

func newStubServer() *stubServer {
	s := &stubServer{routes: make(map[string]http.HandlerFunc)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
		})
		key := r.Method + " " + r.URL.Path
		h, ok := s.routes[key]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	return s
}

func (s *stubServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// jsonResponse returns a handler that writes the given status with a
// JSON body. body may be nil for "204-style" empty responses.
func jsonResponse(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

func makeIntegration(stub *stubServer) *Integration {
	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	integ := NewIntegration(c)
	integ.SetState(StateDegraded) // post-Ping
	return integ
}

func TestBootstrapAdoptsByStoredID(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	stub.handle("GET", "/profiles/p1", jsonResponse(http.StatusOK, map[string]any{
		"profile": map[string]any{
			"_id":       "p1",
			"name":      ManagedProfileName,
			"createdAt": time.Now().UTC(),
		},
	}))

	store := newMemStore()
	_ = store.Set(t.Context(), SettingProfileID, "p1")

	b := NewBootstrapper(makeIntegration(stub), store, "dev")
	b.backoff = nil // skip retries to keep the test fast on failure paths

	if err := b.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.data[SettingProfileID]; got != "p1" {
		t.Errorf("profile_id: got %q want p1", got)
	}
	if b.integ.State() != StateOK {
		t.Errorf("state: got %s want ok", b.integ.State())
	}
}

func TestBootstrapClearsStaleIDOn404ThenAdoptsByName(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	stub.handle("GET", "/profiles/old", jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"}))
	stub.handle("GET", "/profiles", jsonResponse(http.StatusOK, map[string]any{
		"profiles": []map[string]any{
			{"_id": "p2", "name": ManagedProfileName, "createdAt": time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		},
	}))

	store := newMemStore()
	_ = store.Set(t.Context(), SettingProfileID, "old")

	b := NewBootstrapper(makeIntegration(stub), store, "dev")
	b.backoff = nil

	if err := b.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.data[SettingProfileID]; got != "p2" {
		t.Errorf("profile_id: got %q want p2", got)
	}
}

func TestBootstrapCreatesWhenNoMatchInList(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var listCalls atomic.Int32
	stub.handle("GET", "/profiles", func(w http.ResponseWriter, r *http.Request) {
		listCalls.Add(1)
		// First list (pre-create) returns no matches; second list
		// (post-create) returns the freshly-created profile.
		if listCalls.Load() == 1 {
			jsonResponse(http.StatusOK, map[string]any{"profiles": []any{}})(w, r)
			return
		}
		jsonResponse(http.StatusOK, map[string]any{
			"profiles": []map[string]any{
				{"_id": "p3", "name": ManagedProfileName, "createdAt": time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)},
			},
		})(w, r)
	})
	stub.handle("POST", "/profiles", jsonResponse(http.StatusOK, map[string]any{
		"profile": map[string]any{
			"_id":       "p3",
			"name":      ManagedProfileName,
			"createdAt": time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
		},
	}))

	store := newMemStore()
	b := NewBootstrapper(makeIntegration(stub), store, "dev")
	b.backoff = nil

	if err := b.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.data[SettingProfileID]; got != "p3" {
		t.Errorf("profile_id: got %q want p3", got)
	}
}

func TestBootstrapReturnsAuthErrorOn401(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	stub.handle("GET", "/profiles", jsonResponse(http.StatusUnauthorized, map[string]string{"error": "invalid_token"}))

	store := newMemStore()
	integ := makeIntegration(stub)
	b := NewBootstrapper(integ, store, "dev")
	b.backoff = nil

	err := b.Run(t.Context())
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	if integ.State() != StateDisabled {
		t.Errorf("state: got %s want disabled", integ.State())
	}
}

func TestBootstrapAuthHeaderIsPresent(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	stub.handle("GET", "/profiles/p1", jsonResponse(http.StatusOK, map[string]any{
		"profile": map[string]any{"_id": "p1", "name": ManagedProfileName},
	}))

	store := newMemStore()
	_ = store.Set(t.Context(), SettingProfileID, "p1")
	b := NewBootstrapper(makeIntegration(stub), store, "dev")
	b.backoff = nil

	if err := b.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, r := range stub.requests {
		if !strings.HasPrefix(r.Auth, "Bearer ") {
			t.Errorf("missing bearer header on %s %s: got %q", r.Method, r.Path, r.Auth)
		}
	}
}

func TestCreateConnectLinkForwardsRedirectURL(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var sawRedirect, sawProfileID string
	stub.handle("GET", "/connect/linkedin", func(w http.ResponseWriter, r *http.Request) {
		sawRedirect = r.URL.Query().Get("redirect_url")
		sawProfileID = r.URL.Query().Get("profileId")
		jsonResponse(http.StatusOK, map[string]string{"authUrl": "https://zernio.com/oauth/linkedin?token=t"})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{
		Timeout:     time.Second,
		RedirectURL: "https://app.example.com/oauth/done",
	})
	// Empty per-call redirect falls back to the Client's configured RedirectURL.
	got, err := c.CreateConnectLink(t.Context(), "p_test", "linkedin", "")
	if err != nil {
		t.Fatalf("CreateConnectLink: %v", err)
	}
	if got == "" {
		t.Fatalf("authUrl: empty response")
	}
	if sawRedirect != "https://app.example.com/oauth/done" {
		t.Errorf("redirect_url forwarded: got %q want app.example.com/oauth/done", sawRedirect)
	}
	if sawProfileID != "p_test" {
		t.Errorf("profileId forwarded: got %q want p_test", sawProfileID)
	}
}

func TestCreateConnectLinkOmitsRedirectURLWhenUnset(t *testing.T) {
	stub := newStubServer()
	defer stub.Close()
	var redirectKeys []string
	stub.handle("GET", "/connect/linkedin", func(w http.ResponseWriter, r *http.Request) {
		// Capture every key Zernio sees so we can prove redirect_url
		// is absent (rather than just empty-valued).
		for k := range r.URL.Query() {
			redirectKeys = append(redirectKeys, k)
		}
		jsonResponse(http.StatusOK, map[string]string{"authUrl": "https://zernio.com/oauth/linkedin?token=t"})(w, r)
	})

	c := NewClient(StaticKey("test-key"), stub.URL, ClientOpts{Timeout: time.Second})
	if _, err := c.CreateConnectLink(t.Context(), "p_test", "linkedin", ""); err != nil {
		t.Fatalf("CreateConnectLink: %v", err)
	}
	for _, k := range redirectKeys {
		if k == "redirect_url" {
			t.Fatalf("redirect_url was sent without being configured (keys=%v)", redirectKeys)
		}
	}
}

func TestClientStringRedactsAPIKey(t *testing.T) {
	c := NewClient(StaticKey("super-secret-key"), "https://example.com", ClientOpts{Timeout: time.Second})
	got := fmt.Sprintf("%v", c)
	if strings.Contains(got, "super-secret-key") {
		t.Fatalf("API key leaked in String(): %q", got)
	}
}
