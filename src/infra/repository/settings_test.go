package repository_test

import (
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestListTenantIDsByKey covers the cross-tenant enumeration the per-tenant
// Zernio sync worker uses (CON-100): under a system context it returns the
// distinct tenants that have a non-empty value for the key, ignoring empty
// values and other keys. openMigratedDB bypasses FK enforcement, so synthetic
// tenant_ids need no tenants rows.
func TestListTenantIDsByKey(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewSettingRepository(db)

	upsert := func(tenant, key, value string) {
		t.Helper()
		ctx := tenantctx.With(t.Context(), tenant)
		if err := repo.Upsert(ctx, &models.Setting{Key: key, Value: value}); err != nil {
			t.Fatalf("upsert %s/%s: %v", tenant, key, err)
		}
	}
	upsert("ta", "zernio.profile_id", "p1")
	upsert("tb", "zernio.profile_id", "p2")
	upsert("tc", "zernio.profile_id", "") // empty value → excluded
	upsert("td", "other.key", "x")        // different key → excluded

	sys := tenantctx.WithSystem(t.Context())
	ids, err := repo.ListTenantIDsByKey(sys, "zernio.profile_id")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	slices.Sort(ids)
	want := []string{"ta", "tb"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("got %v want %v", ids, want)
	}
}
