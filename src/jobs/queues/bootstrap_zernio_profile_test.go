package queues_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// profileStub is a minimal Zernio /profiles server for the bootstrap job tests.
// listStatus/createStatus drive the failure cases; createNames captures the
// names POSTed so the rename (CON-102 FR6) can be asserted.
type profileStub struct {
	mu           sync.Mutex
	requests     int
	createNames  []string
	listStatus   int // status for GET /profiles (0 → 200 with empty list)
	createStatus int // status for POST /profiles (0 → 200 with a created profile)
}

func (s *profileStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/profiles":
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"profiles": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/profiles":
			body, _ := io.ReadAll(r.Body)
			var in struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(body, &in)
			s.mu.Lock()
			s.createNames = append(s.createNames, in.Name)
			s.mu.Unlock()
			if s.createStatus != 0 {
				w.WriteHeader(s.createStatus)
				return
			}
			now := time.Now().UTC().Format(time.RFC3339)
			writeJSON(w, http.StatusOK, map[string]any{
				"profile": map[string]any{"_id": "prof-" + in.Name, "name": in.Name, "createdAt": now, "updatedAt": now},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func (s *profileStub) reqCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *profileStub) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.createNames...)
}

// newBootstrapProcessor wires a real Bootstrapper + Integration around the stub,
// returning the processor under test and the in-memory settings store so a test
// can assert the persisted profile id. state seeds the integration state
// (StateDisabled models "no key"; StateDegraded/OK model "enabled").
func newBootstrapProcessor(stubURL string, state zernio.State, store zernio.SettingsStore) *queues.BootstrapZernioProfileProcessor {
	integ := zernio.NewIntegration(zernio.NewClient(zernio.StaticKey("k"), stubURL, zernio.ClientOpts{Timeout: 5 * time.Second}))
	integ.SetState(state)
	return &queues.BootstrapZernioProfileProcessor{
		Integration:  integ,
		Bootstrapper: zernio.NewBootstrapper(integ, store, "dev"),
	}
}

func bootstrapJob(tenantID string) *river.Job[queues.BootstrapZernioProfileTask] {
	return &river.Job[queues.BootstrapZernioProfileTask]{Args: queues.BootstrapZernioProfileTask{TenantID: tenantID}}
}

func TestBootstrapJobProvisionsNamedProfile(t *testing.T) {
	stub := &profileStub{}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	p := newBootstrapProcessor(srv.URL, zernio.StateDegraded, store)

	if err := p.Work(t.Context(), bootstrapJob("acme")); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// The profile was created and its id persisted to the tenant's settings.
	id, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID)
	if !ok || id == "" {
		t.Fatalf("expected profile_id persisted, got ok=%v id=%q", ok, id)
	}
	// FR6: the profile name is "Ogen-<env>-<tenant_id>".
	if names := stub.names(); len(names) != 1 || names[0] != "Ogen-dev-acme" {
		t.Fatalf("created profile names = %v; want [\"Ogen-dev-acme\"]", names)
	}
}

func TestBootstrapJobSkipsWhenDisabled(t *testing.T) {
	stub := &profileStub{}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	// StateDisabled models "no API key" → Integration.Enabled() is false.
	p := newBootstrapProcessor(srv.URL, zernio.StateDisabled, store)

	if err := p.Work(t.Context(), bootstrapJob("acme")); err != nil {
		t.Fatalf("Work returned error; disabled integration must be a clean no-op: %v", err)
	}
	if n := stub.reqCount(); n != 0 {
		t.Fatalf("expected no Zernio calls when disabled, got %d", n)
	}
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); ok {
		t.Fatalf("expected no profile persisted when disabled")
	}
}

func TestBootstrapJobReturnsErrorOnNonAuthFailure(t *testing.T) {
	// A 403 stands in for any non-401 failure: it leaves the integration
	// degraded (not disabled), so Work surfaces the error for River to retry.
	// (403 short-circuits the bootstrapper's internal backoff, keeping the test
	// fast; a 5xx would exercise the same Work branch but after ~15s of retries.)
	stub := &profileStub{listStatus: http.StatusForbidden}
	srv := stub.server()
	defer srv.Close()

	p := newBootstrapProcessor(srv.URL, zernio.StateDegraded, newFakeSettings())

	if err := p.Work(t.Context(), bootstrapJob("acme")); err == nil {
		t.Fatalf("expected an error on a degraded (non-auth) failure so River retries")
	}
}

func TestBootstrapJobGivesUpOn401(t *testing.T) {
	stub := &profileStub{listStatus: http.StatusUnauthorized}
	srv := stub.server()
	defer srv.Close()

	p := newBootstrapProcessor(srv.URL, zernio.StateDegraded, newFakeSettings())

	// A rejected key flips the integration to disabled; the lazy on-connect path
	// remains the fallback, so the job gives up cleanly (no error → no retry).
	if err := p.Work(t.Context(), bootstrapJob("acme")); err != nil {
		t.Fatalf("expected a clean give-up (nil) on 401, got: %v", err)
	}
}
