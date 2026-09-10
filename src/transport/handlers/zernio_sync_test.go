package handlers_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestZernioPerTenantSyncIsolation proves the per-tenant sync sweep (CON-100):
// each tenant is synced under its own context against its own Zernio profile, so
// social_accounts land in — and are visible only within — the owning tenant.
func TestZernioPerTenantSyncIsolation(t *testing.T) {
	db := mustOpenTestDBWithMigrations()

	stub := newZernioStub()
	defer stub.Close()
	acct := func(id, profile, platform, username string) map[string]any {
		now := time.Now().UTC().Format(time.RFC3339)
		return map[string]any{
			"_id": id, "profileId": profile, "platform": platform,
			"username": username, "displayName": username, "isActive": true,
			"createdAt": now, "updatedAt": now,
		}
	}
	// /accounts is profile-scoped: each profile returns its own account.
	stub.handle("GET", "/accounts", func(w http.ResponseWriter, r *http.Request) {
		var accts []any
		switch r.URL.Query().Get("profileId") {
		case "p-a":
			accts = []any{acct("acc-a", "p-a", "linkedin", "alice")}
		case "p-b":
			accts = []any{acct("acc-b", "p-b", "linkedin", "bob")}
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": accts})
	})
	profileHandler := func(id string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			now := time.Now().UTC().Format(time.RFC3339)
			writeJSON(w, http.StatusOK, map[string]any{
				"profile": map[string]any{"_id": id, "name": "Ogen", "createdAt": now, "updatedAt": now},
			})
		}
	}
	stub.handle("GET", "/profiles/p-a", profileHandler("p-a"))
	stub.handle("GET", "/profiles/p-b", profileHandler("p-b"))

	settingRepo := repository.NewSettingRepository(db)
	accountRepo := repository.NewSocialAccountRepository(db)
	client := zernio.NewClient(zernio.StaticKey("k"), stub.URL, zernio.ClientOpts{Timeout: time.Second})
	integ := zernio.NewIntegration(client)
	store := &settingStoreFromRepo{repo: settingRepo}
	worker := zernio.NewWorker(integ, accountRepo, store, &quietHub{}, zernio.NewBootstrapper(integ, store, "dev"), nil,
		time.Hour, time.Minute,
		func(ctx context.Context) ([]string, error) {
			return settingRepo.ListTenantIDsByKey(ctx, zernio.SettingProfileID)
		})

	// Two tenants, each with its own profile. 'default' exists from the
	// migration; create the second (tenants is not auto-scoped).
	if _, err := db.NewInsert().Model(&models.Tenant{ID: "tb", Name: "Beta", Slug: "beta", TierID: models.DefaultTierID}).
		Exec(t.Context()); err != nil {
		t.Fatalf("create tenant tb: %v", err)
	}
	ctxA := tenantctx.With(t.Context(), models.DefaultTenantID)
	ctxB := tenantctx.With(t.Context(), "tb")
	mustUpsertSetting(t, settingRepo, ctxA, zernio.SettingProfileID, "p-a")
	mustUpsertSetting(t, settingRepo, ctxB, zernio.SettingProfileID, "p-b")

	if err := worker.SyncOnce(tenantctx.WithSystem(t.Context())); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Tenant A got only p-a's account, stamped to the default tenant.
	a, err := accountRepo.ListAll(ctxA, "p-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].ID != "acc-a" || a[0].TenantID != models.DefaultTenantID {
		t.Fatalf("tenant A accounts: %+v", a)
	}
	// Tenant B got only p-b's account, stamped to tenant tb.
	b, err := accountRepo.ListAll(ctxB, "p-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || b[0].ID != "acc-b" || b[0].TenantID != "tb" {
		t.Fatalf("tenant B accounts: %+v", b)
	}
	// Tenant A cannot see tenant B's account.
	cross, err := accountRepo.ListAll(ctxA, "p-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(cross) != 0 {
		t.Fatalf("tenant A leaked tenant B's accounts: %+v", cross)
	}
}

// TestZernioSyncSelfHealsDegraded proves the worker promotes a stale app-wide
// StateDegraded back to ok on a successful sync. A transient boot-time Ping
// failure leaves the instance degraded, and the warmup never re-runs without a
// key change — so a clean sync tick (which proves the key + profile work) is the
// recovery path. Regression guard for the "stuck degraded" bug.
func TestZernioSyncSelfHealsDegraded(t *testing.T) {
	db := mustOpenTestDBWithMigrations()

	stub := newZernioStub()
	defer stub.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	stub.handle("GET", "/accounts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{}})
	})
	stub.handle("GET", "/profiles/p-a", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"profile": map[string]any{"_id": "p-a", "name": "Ogen", "createdAt": now, "updatedAt": now},
		})
	})

	settingRepo := repository.NewSettingRepository(db)
	accountRepo := repository.NewSocialAccountRepository(db)
	client := zernio.NewClient(zernio.StaticKey("k"), stub.URL, zernio.ClientOpts{Timeout: time.Second})
	integ := zernio.NewIntegration(client)
	// Simulate the stuck instance: the key is valid, but a transient boot-time
	// Ping failure left the app-wide state pinned to degraded.
	integ.SetState(zernio.StateDegraded)

	store := &settingStoreFromRepo{repo: settingRepo}
	worker := zernio.NewWorker(integ, accountRepo, store, &quietHub{}, zernio.NewBootstrapper(integ, store, "dev"), nil,
		time.Hour, time.Minute,
		func(ctx context.Context) ([]string, error) {
			return settingRepo.ListTenantIDsByKey(ctx, zernio.SettingProfileID)
		})

	ctxA := tenantctx.With(t.Context(), models.DefaultTenantID)
	mustUpsertSetting(t, settingRepo, ctxA, zernio.SettingProfileID, "p-a")

	if err := worker.SyncOnce(tenantctx.WithSystem(t.Context())); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := integ.State(); got != zernio.StateOK {
		t.Fatalf("expected integration to self-heal degraded→ok, got %s", got)
	}
}

func mustUpsertSetting(t *testing.T, repo repository.SettingRepository, ctx context.Context, key, val string) {
	t.Helper()
	if err := repo.Upsert(ctx, &models.Setting{Key: key, Value: val}); err != nil {
		t.Fatalf("upsert %s: %v", key, err)
	}
}
