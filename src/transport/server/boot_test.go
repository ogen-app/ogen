package server

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

type countingTenants struct {
	repository.TenantRepository
	reads int
}

func (c *countingTenants) GetByID(_ context.Context, id string) (*models.Tenant, error) {
	c.reads++
	return &models.Tenant{ID: id, TierID: "pro"}, nil
}

func TestTenantTierOfCachesTenantTiers(t *testing.T) {
	tenants := &countingTenants{}
	tierOf := tenantTierOf(&repos{tenantRepo: tenants})
	ctx := tenantctx.With(t.Context(), "tn-1")

	for range 3 {
		if tier, ok := tierOf(ctx); !ok || tier != "pro" {
			t.Fatalf("tier = (%q, %v), want (pro, true)", tier, ok)
		}
	}
	if tenants.reads != 1 {
		t.Fatalf("tenant reads = %d, want 1", tenants.reads)
	}

	if tier, ok := tierOf(tenantctx.WithTier(ctx, "trial")); !ok || tier != "trial" {
		t.Fatalf("stamped tier = (%q, %v), want (trial, true)", tier, ok)
	}
	if _, ok := tierOf(t.Context()); ok {
		t.Fatal("no tenant in context must resolve no tier")
	}
}
