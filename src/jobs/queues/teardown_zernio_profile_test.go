package queues_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// deleteFailSettings is a zernio.SettingsStore that reads/writes normally but
// fails every Delete, modelling a transient settings-store outage during the
// post-delete local cleanup.
type deleteFailSettings struct{ *fakeSettings }

func (deleteFailSettings) Delete(context.Context, string) error {
	return errors.New("settings store unavailable")
}

// teardownStub is a minimal Zernio server for the teardown job tests. It serves
// GET /accounts (the accounts to disconnect), DELETE /accounts/{id}, and
// DELETE /profiles/{id}, recording each call so a test can assert the
// disconnect-then-delete ordering and idempotent no-ops. Status overrides drive
// the failure cases.
type teardownStub struct {
	mu sync.Mutex

	accounts []string // account ids returned by GET /accounts

	deletedAccounts []string // account ids hit by DELETE /accounts/{id}
	deletedProfiles []string // profile ids hit by DELETE /profiles/{id}

	deleteProfileStatus int // status for DELETE /profiles/{id} (0 → 200)
	deleteAccountStatus int // status for DELETE /accounts/{id} (0 → 200)
	listAccountsStatus  int // status for GET /accounts (0 → 200)
}

func (s *teardownStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/accounts":
			s.mu.Lock()
			status, accts := s.listAccountsStatus, append([]string(nil), s.accounts...)
			s.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			out := make([]map[string]any, 0, len(accts))
			for _, id := range accts {
				out = append(out, map[string]any{"_id": id, "platform": "x", "profileId": r.URL.Query().Get("profileId")})
			}
			writeJSON(w, http.StatusOK, map[string]any{"accounts": out})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/accounts/"):
			id := strings.TrimPrefix(r.URL.Path, "/accounts/")
			s.mu.Lock()
			s.deletedAccounts = append(s.deletedAccounts, id)
			status := s.deleteAccountStatus
			s.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "ok"})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/profiles/"):
			id := strings.TrimPrefix(r.URL.Path, "/profiles/")
			s.mu.Lock()
			s.deletedProfiles = append(s.deletedProfiles, id)
			status := s.deleteProfileStatus
			s.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "Profile deleted successfully"})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func (s *teardownStub) profileDeletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletedProfiles...)
}

func (s *teardownStub) accountDeletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletedAccounts...)
}

// fakeTenantStatus is a TenantStatusReader returning a fixed status, modelling a
// soft-deleted tenant ("deleted") so teardown proceeds, or an "active" one to
// exercise the restore-guard.
type fakeTenantStatus struct{ status string }

func (f fakeTenantStatus) GetStatus(context.Context, string) (string, error) {
	return f.status, nil
}

// newTeardownProcessor wires the processor under test around the stub, seeding a
// deleted-tenant status reader by default so teardown runs.
func newTeardownProcessor(stubURL string, store zernio.SettingsStore, tenants queues.TenantStatusReader) *queues.TeardownZernioProfileProcessor {
	integ := zernio.NewIntegration(zernio.NewClient(zernio.StaticKey("k"), stubURL, zernio.ClientOpts{Timeout: 5 * time.Second}))
	integ.SetState(zernio.StateOK)
	return &queues.TeardownZernioProfileProcessor{
		Integration: integ,
		Settings:    store,
		Tenants:     tenants,
	}
}

func teardownJob(tenantID string) *river.Job[queues.TeardownZernioProfileTask] {
	return &river.Job[queues.TeardownZernioProfileTask]{Args: queues.TeardownZernioProfileTask{TenantID: tenantID}}
}

// seedTeardownProfile records a tenant's Zernio profile id so the teardown job
// resolves it, mirroring what the bootstrapper would have persisted.
func seedTeardownProfile(store zernio.SettingsStore, tenantID, profileID string) {
	_ = store.Set(tctx(tenantID), zernio.SettingProfileID, profileID)
}

func TestTeardownDisconnectsAccountsThenDeletesProfile(t *testing.T) {
	stub := &teardownStub{accounts: []string{"acc-1", "acc-2"}}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	seedTeardownProfile(store, "acme", "prof-acme")
	p := newTeardownProcessor(srv.URL, store, fakeTenantStatus{status: models.TenantStatusDeleted})

	if err := p.Work(t.Context(), teardownJob("acme")); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// Both accounts disconnected (clearing Zernio's active-account 400 block) and
	// the profile deleted.
	if got := stub.accountDeletes(); len(got) != 2 {
		t.Fatalf("disconnected accounts = %v; want 2", got)
	}
	if got := stub.profileDeletes(); len(got) != 1 || got[0] != "prof-acme" {
		t.Fatalf("deleted profiles = %v; want [prof-acme]", got)
	}
	// The local profile pointer is cleared, so a re-run is a clean no-op.
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); ok {
		t.Fatalf("expected profile_id cleared after teardown")
	}
}

func TestTeardownNoProfileIsNoop(t *testing.T) {
	stub := &teardownStub{}
	srv := stub.server()
	defer srv.Close()

	// No seeded profile id → nothing provisioned → no Zernio calls.
	p := newTeardownProcessor(srv.URL, newFakeSettings(), fakeTenantStatus{status: models.TenantStatusDeleted})

	if err := p.Work(t.Context(), teardownJob("acme")); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := stub.profileDeletes(); len(got) != 0 {
		t.Fatalf("expected no profile delete when nothing provisioned, got %v", got)
	}
}

func TestTeardownProfileAlreadyGoneIsIdempotent(t *testing.T) {
	// A 404 on DELETE /profiles means the profile is already gone — a re-run or a
	// double delete must succeed as a no-op.
	stub := &teardownStub{deleteProfileStatus: http.StatusNotFound}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	seedTeardownProfile(store, "acme", "prof-acme")
	p := newTeardownProcessor(srv.URL, store, fakeTenantStatus{status: models.TenantStatusDeleted})

	if err := p.Work(t.Context(), teardownJob("acme")); err != nil {
		t.Fatalf("expected 404 profile-delete to be an idempotent no-op, got: %v", err)
	}
	// The pointer is still cleared so we don't re-attempt a doomed delete forever.
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); ok {
		t.Fatalf("expected profile_id cleared even on 404")
	}
}

func TestTeardownProfileGoneOnListIsCleanedUp(t *testing.T) {
	// A retry after a prior attempt already deleted the profile upstream but
	// failed to clear settings: GET /accounts 404s (profile gone). The job must
	// treat that as already-deleted and still clear the local profile keys,
	// rather than erroring on every attempt until it dies.
	stub := &teardownStub{listAccountsStatus: http.StatusNotFound}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	seedTeardownProfile(store, "acme", "prof-acme")
	p := newTeardownProcessor(srv.URL, store, fakeTenantStatus{status: models.TenantStatusDeleted})

	if err := p.Work(t.Context(), teardownJob("acme")); err != nil {
		t.Fatalf("expected a 404 on ListAccounts to converge to cleanup, got: %v", err)
	}
	if got := stub.profileDeletes(); len(got) != 0 {
		t.Fatalf("expected no profile delete when it is already gone, got %v", got)
	}
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); ok {
		t.Fatalf("expected profile_id cleared even when the profile was already gone")
	}
}

func TestTeardownRetriesWhenLocalCleanupFails(t *testing.T) {
	// The remote deletes succeed but clearing the local profile keys fails. Work
	// must surface the error so River retries — otherwise a dangling profile_id
	// survives a "successful" teardown.
	stub := &teardownStub{accounts: []string{"acc-1"}}
	srv := stub.server()
	defer srv.Close()

	inner := newFakeSettings()
	seedTeardownProfile(inner, "acme", "prof-acme")
	store := deleteFailSettings{fakeSettings: inner}
	p := newTeardownProcessor(srv.URL, store, fakeTenantStatus{status: models.TenantStatusDeleted})

	if err := p.Work(t.Context(), teardownJob("acme")); err == nil {
		t.Fatalf("expected an error when local cleanup fails so River retries")
	}
	// The remote profile was still deleted (cleanup runs after) — a retry will
	// 404 on ListAccounts and re-attempt only the local clear.
	if got := stub.profileDeletes(); len(got) != 1 {
		t.Fatalf("expected the profile delete to have happened, got %v", got)
	}
}

func TestTeardownRetriesOnZernioError(t *testing.T) {
	// A 5xx on DELETE /profiles is transient — Work returns an error so River
	// retries with backoff. The delete-response never blocks on this (it happens
	// in the worker), so retrying is free.
	stub := &teardownStub{deleteProfileStatus: http.StatusInternalServerError}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	seedTeardownProfile(store, "acme", "prof-acme")
	p := newTeardownProcessor(srv.URL, store, fakeTenantStatus{status: models.TenantStatusDeleted})

	if err := p.Work(t.Context(), teardownJob("acme")); err == nil {
		t.Fatalf("expected an error on a 5xx so River retries")
	}
	// The pointer must survive a failed teardown so the retry can find the profile.
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); !ok {
		t.Fatalf("expected profile_id retained after a failed teardown")
	}
}

func TestTeardownSkipsRestoredTenant(t *testing.T) {
	// The tenant was restored (active again) between the soft-delete enqueue and
	// this run — deleting its profile would orphan a live workspace, so skip.
	stub := &teardownStub{accounts: []string{"acc-1"}}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	seedTeardownProfile(store, "acme", "prof-acme")
	p := newTeardownProcessor(srv.URL, store, fakeTenantStatus{status: models.TenantStatusActive})

	if err := p.Work(t.Context(), teardownJob("acme")); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := stub.profileDeletes(); len(got) != 0 {
		t.Fatalf("expected no delete for a restored (active) tenant, got %v", got)
	}
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); !ok {
		t.Fatalf("expected profile_id retained for a restored tenant")
	}
}
