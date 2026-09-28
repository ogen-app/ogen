package server

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/config"
)

// entitlementDeps is the versioned tier-entitlement layer: the embedded
// feature catalog, the point-in-time resolver, and the quota limiter.
type entitlementDeps struct {
	catalog  *entitlements.Catalog
	resolver *entitlements.Resolver
	limiter  *entitlements.Limiter
}

// newEntitlements loads the feature catalog (a malformed embed fails boot) and
// builds the resolver and limiter over it.
func newEntitlements(cfg *config.Config, r *repos) (entitlementDeps, error) {
	catalog, err := entitlements.LoadCatalog()
	if err != nil {
		return entitlementDeps{}, err
	}
	resolver := entitlements.NewResolver(r.tierVersionRepo, r.tierAssignmentRepo, r.tenantRepo, catalog)
	return entitlementDeps{
		catalog:  catalog,
		resolver: resolver,
		limiter:  newEntitlementLimiter(cfg, r, resolver, catalog),
	}, nil
}

// newEntitlementLimiter registers a control-plane counter for each capped
// feature. The same counters back the "N of M" usage on GET
// /api/me/entitlements.
func newEntitlementLimiter(cfg *config.Config, r *repos, resolver *entitlements.Resolver, catalog *entitlements.Catalog) *entitlements.Limiter {
	return entitlements.NewLimiter(resolver, catalog, entitlements.ParseMode(cfg.EntitlementEnforcementMode)).
		Register("team_seats", tenantScoped(r.userRepo.CountInTenant)).
		Register("active_campaigns", tenantScoped(r.campaignRepo.CountActive)).
		Register("content_bank_assets", tenantScoped(r.pieceRepo.Count)).
		// A stricter sub-cap on the bank: only URL-type assets count.
		Register("web_page_imports", tenantScoped(func(ctx context.Context) (int64, error) {
			return r.pieceRepo.CountByType(ctx, models.AssetTypeURL)
		})).
		Register("media_storage_bytes", tenantScoped(r.mediaStorageBytes))
}

// tenantScoped adapts a tenant-scoped repository count to a CounterFunc. The
// explicit tenant argument is ignored because ctx already carries the tenant.
func tenantScoped(count func(context.Context) (int64, error)) entitlements.CounterFunc {
	return func(ctx context.Context, _ string) (int64, error) { return count(ctx) }
}

// mediaStorageBytes is all uploaded media in the tenant: post attachments plus
// content-bank originals.
func (r *repos) mediaStorageBytes(ctx context.Context) (int64, error) {
	att, err := r.postAttachmentRepo.SumSizeBytesInTenant(ctx)
	if err != nil {
		return 0, err
	}
	bank, err := r.assetFileRepo.SumSizeBytesInTenant(ctx)
	if err != nil {
		return 0, err
	}
	return att + bank, nil
}
