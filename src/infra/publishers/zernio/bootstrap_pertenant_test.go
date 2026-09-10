package zernio

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestBootstrapPerTenantProfileNaming proves CON-100: the bootstrapper names
// each tenant's profile after its tenant_id (from the context), so two tenants
// bootstrap distinct Zernio profiles and each profile_id is stored under its
// own tenant.
func TestBootstrapPerTenantProfileNaming(t *testing.T) {
	stub := newStubServer()
	// No existing profiles → always create.
	stub.handle("GET", "/profiles", jsonResponse(http.StatusOK, map[string]any{"profiles": []any{}}))

	var mu sync.Mutex
	var createBodies []string
	n := 0
	stub.handle("POST", "/profiles", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		createBodies = append(createBodies, string(body))
		n++
		id := fmt.Sprintf("prof-%d", n)
		mu.Unlock()
		now := time.Now().UTC().Format(time.RFC3339)
		jsonResponse(http.StatusOK, map[string]any{
			"profile": map[string]any{"_id": id, "name": "x", "createdAt": now, "updatedAt": now},
		})(w, r)
	})

	integ := makeIntegration(stub)
	integ.SetState(StateDegraded) // enabled
	store := &tenantMemStore{data: map[string]string{}}
	b := NewBootstrapper(integ, store, "dev")

	ctxA := tenantctx.With(t.Context(), "acme")
	ctxB := tenantctx.With(t.Context(), "beta")
	if err := b.Run(ctxA); err != nil {
		t.Fatalf("run A: %v", err)
	}
	if err := b.Run(ctxB); err != nil {
		t.Fatalf("run B: %v", err)
	}

	// Each tenant got its own profile id, stored under its own tenant
	// (tenantMemStore is keyed by tenant, so a leak would show as equal ids).
	idA, okA, _ := store.Get(ctxA, SettingProfileID)
	idB, okB, _ := store.Get(ctxB, SettingProfileID)
	if !okA || !okB || idA == "" || idB == "" || idA == idB {
		t.Fatalf("expected distinct per-tenant profile ids, got A=%q B=%q", idA, idB)
	}

	// The two create calls carried tenant-specific names:
	// "Ogen-<env>-<tenant_id>" (CON-102 FR6).
	mu.Lock()
	defer mu.Unlock()
	if len(createBodies) != 2 ||
		!strings.Contains(createBodies[0], `"Ogen-dev-acme"`) ||
		!strings.Contains(createBodies[1], `"Ogen-dev-beta"`) {
		t.Fatalf("create bodies = %v; want \"Ogen-<env>-<tenant_id>\" names", createBodies)
	}
}
