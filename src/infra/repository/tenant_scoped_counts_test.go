package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const (
	countTenantA = "tn-counts-a"
	countTenantB = "tn-counts-b"
)

// openCountTenants returns a context per tenant on a migrated DB, clearing
// both tenants' rows when the test ends.
func openCountTenants(t *testing.T) (db *bun.DB, a, b context.Context) {
	t.Helper()
	db = openMigratedDB(t)
	t.Cleanup(func() {
		tenants := bun.List([]string{countTenantA, countTenantB})
		for _, table := range []string{"campaigns", "assets", "brand_guardrails", "brand_guardrails_stance", "auto_publish_allowlist"} {
			_, _ = db.NewDelete().TableExpr(table).Where("tenant_id IN (?)", tenants).Exec(context.Background())
		}
	})
	return db, tenantctx.With(t.Context(), countTenantA), tenantctx.With(t.Context(), countTenantB)
}

func seedCountAsset(t *testing.T, db *bun.DB, ctx context.Context, id string, typ *string) {
	t.Helper()
	now := time.Now().UTC()
	a := &models.Asset{ID: id, Title: id, Status: models.AssetStatusReady, Type: typ, CreatedBy: "u", CreatedAt: now, UpdatedAt: now}
	if _, err := db.NewInsert().Model(a).Exec(ctx); err != nil {
		t.Fatalf("seed asset %s: %v", id, err)
	}
}

// The entitlement counters count only the context's tenant: another tenant's
// campaigns and Content Bank items never reach its plan read or its limits.
func TestEntitlementCountersCountOneTenant(t *testing.T) {
	db, a, b := openCountTenants(t)
	campaigns := repository.NewCampaignRepository(db, nil, nil, nil)
	assets := repository.NewAssetRepository(db, nil, nil)

	seedCampaign(t, campaigns, a, "a live 1")
	seedCampaign(t, campaigns, a, "a live 2")
	archived := seedCampaign(t, campaigns, a, "a archived")
	if _, err := db.NewUpdate().TableExpr("campaigns").Set("archived_at = now()").Where("id = ?", archived).Exec(a); err != nil {
		t.Fatalf("archive: %v", err)
	}
	seedCampaign(t, campaigns, b, "b live")

	seedCountAsset(t, db, a, "as-counts-a1", new(models.AssetTypeURL))
	seedCountAsset(t, db, a, "as-counts-a2", new(models.AssetTypeURL))
	seedCountAsset(t, db, a, "as-counts-a3", new(models.AssetTypePDF))
	seedCountAsset(t, db, b, "as-counts-b1", new(models.AssetTypeURL))

	for _, tc := range []struct {
		name                     string
		ctx                      context.Context
		campaigns, assets, pages int64
	}{
		{"tenant a", a, 2, 3, 2},
		{"tenant b", b, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n, err := campaigns.CountActive(tc.ctx); err != nil || n != tc.campaigns {
				t.Errorf("CountActive = %d, %v; want %d", n, err, tc.campaigns)
			}
			if n, err := assets.Count(tc.ctx); err != nil || n != tc.assets {
				t.Errorf("Count = %d, %v; want %d", n, err, tc.assets)
			}
			if n, err := assets.CountByType(tc.ctx, models.AssetTypeURL); err != nil || n != tc.pages {
				t.Errorf("CountByType(url) = %d, %v; want %d", n, err, tc.pages)
			}
		})
	}

	// A fresh tenant reads zero everywhere.
	fresh := tenantctx.With(t.Context(), "tn-counts-fresh")
	if n, err := campaigns.CountActive(fresh); err != nil || n != 0 {
		t.Errorf("fresh CountActive = %d, %v; want 0", n, err)
	}
	if n, err := assets.Count(fresh); err != nil || n != 0 {
		t.Errorf("fresh Count = %d, %v; want 0", n, err)
	}
}

// A count without a tenant fails closed rather than counting every tenant.
func TestEntitlementCountersNeedTenant(t *testing.T) {
	db := openMigratedDB(t)
	if _, err := repository.NewCampaignRepository(db, nil, nil, nil).CountActive(t.Context()); err == nil {
		t.Fatal("CountActive without a tenant: want an error")
	}
}

// Another tenant's guardrails don't block this tenant's "no guardrails" stance.
func TestGuardrailsStanceIgnoresOtherTenant(t *testing.T) {
	db, a, b := openCountTenants(t)
	brand := repository.NewBrandRepository(db)

	g := &models.BrandGuardrails{ID: "bg-counts-a", MayClaim: models.StringSlice{}, NeverClaim: models.StringSlice{},
		BannedWords: models.StringSlice{"cheap"}}
	if _, err := brand.SaveGuardrails(a, g, nil, nil); err != nil {
		t.Fatalf("save a's guardrails: %v", err)
	}
	if _, err := brand.SetGuardrailsStance(b, &models.BrandGuardrailsStanceRecord{}); err != nil {
		t.Fatalf("b's stance: %v", err)
	}
	if _, err := brand.SetGuardrailsStance(a, &models.BrandGuardrailsStanceRecord{}); !errors.Is(err, repository.ErrGuardrailsExist) {
		t.Fatalf("a's stance over its own guardrails: err = %v, want ErrGuardrailsExist", err)
	}
}

// One tenant's auto-publish allowlist never routes another tenant's posts.
func TestAutoPublishAllowlistContainsOneTenant(t *testing.T) {
	db, a, b := openCountTenants(t)
	allowlist := repository.NewAutoPublishAllowlistRepository(db)
	if err := allowlist.Set(a, []string{"linkedin"}); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if ok, err := allowlist.Contains(a, "linkedin"); err != nil || !ok {
		t.Errorf("a contains linkedin = %v, %v; want true", ok, err)
	}
	if ok, err := allowlist.Contains(b, "linkedin"); err != nil || ok {
		t.Errorf("b contains linkedin = %v, %v; want false", ok, err)
	}
}
