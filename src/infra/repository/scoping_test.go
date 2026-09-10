package repository_test

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestScopedRepoFailsClosedWithoutTenant is the regression guard for CON-97
// AC12 / §6: a tenant-owned repository query issued with no tenant in context
// must refuse to run (fail closed) rather than touch every tenant's rows. The
// same query succeeds once a tenant is present.
func TestScopedRepoFailsClosedWithoutTenant(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewTagRepository(db)

	if _, err := repo.List(t.Context()); !errors.Is(err, tenantctx.ErrNoTenant) {
		t.Fatalf("expected fail-closed ErrNoTenant for an unscoped read, got %v", err)
	}

	if _, err := repo.List(tenantCtx()); err != nil {
		t.Fatalf("scoped read should succeed, got %v", err)
	}
}
