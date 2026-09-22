package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// TestLimiterEnforcesTrialCap wires the real resolver to the Limiter and proves
// a Trial-tier tenant's active_campaigns cap (1, from the seeded trial-v1) is
// enforced. The counter is controllable so the test does not need to build a
// full campaign graph — the point under test is resolver value -> limiter
// decision.
func TestLimiterEnforcesTrialCap(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()

	tenantRepo := repository.NewTenantRepository(db)
	tn := &models.Tenant{ID: "tn-trial", Name: "Trial Co", Slug: "trial-co", TierID: "trial"}
	if err := tenantRepo.Create(ctx, tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	resolver := entitlements.NewResolver(
		repository.NewTenantTierVersionRepository(db),
		repository.NewTenantTierAssignmentRepository(db),
		tenantRepo, cat)

	var current int64
	lim := entitlements.NewLimiter(resolver, cat, entitlements.ModeEnforce).
		Register("active_campaigns", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return current, nil }))

	// Under the cap → allowed.
	if _, err := lim.Require(ctx, tn.ID, "active_campaigns"); err != nil {
		t.Fatalf("under cap should allow: %v", err)
	}
	// At the cap (1) → denied with a typed quota error carrying limit + current.
	current = 1
	_, err = lim.Require(ctx, tn.ID, "active_campaigns")
	var qe *entitlements.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("at cap should deny with *QuotaExceededError, got %v", err)
	}
	if qe.Limit != 1 || qe.Current != 1 || qe.Key != "active_campaigns" {
		t.Fatalf("unexpected quota error: %+v", qe)
	}

	// warn mode never blocks, even over cap.
	warn := entitlements.NewLimiter(resolver, cat, entitlements.ModeWarn).
		Register("active_campaigns", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return 9, nil }))
	if _, err := warn.Require(ctx, tn.ID, "active_campaigns"); err != nil {
		t.Fatalf("warn mode should allow: %v", err)
	}
}

// TestEntitlementCountersValidSQL runs each CON-295 count query against the real
// schema (they must not error), guarding against column/table drift.
func TestEntitlementCountersValidSQL(t *testing.T) {
	db := pgtest.MustDB()
	ctx := tenantctx.With(t.Context(), models.DefaultTenantID)

	if _, err := repository.NewCampaignRepository(db, nil, nil, nil).CountActive(ctx); err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if _, err := repository.NewAssetRepository(db, nil, nil).Count(ctx); err != nil {
		t.Fatalf("Asset Count: %v", err)
	}
	if _, err := repository.NewAssetRepository(db, nil, nil).CountByType(ctx, models.AssetTypeURL); err != nil {
		t.Fatalf("Asset CountByType: %v", err)
	}
	if _, err := repository.NewUserRepository(db).CountInTenant(ctx); err != nil {
		t.Fatalf("CountInTenant: %v", err)
	}
	if _, err := repository.NewPostAttachmentRepository(db).SumSizeBytesInTenant(ctx); err != nil {
		t.Fatalf("SumSizeBytesInTenant: %v", err)
	}
}
