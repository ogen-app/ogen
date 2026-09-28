package server

import (
	"context"
	"log/slog"
	"slices"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/platforms"
	pubzernio "github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// loadOperatorCatalogs loads the operator-controlled platform catalog, global
// upload/thread limits and per-flow model configuration into their process
// caches, each kept fresh in the background. A failed initial load is
// non-fatal: the background refresh retries.
func loadOperatorCatalogs(ctx context.Context, cfg *config.Config, r *repos) {
	pubzernio.InitCatalog(ctx, r.platformRepo)
	platforms.InitGlobalLimits(ctx, r.platformGlobalLimitsRepo)
	// The config-field defaults seed any missing global-default row, so a
	// fresh database resolves exactly the models the environment names.
	modelconfig.Init(ctx, r.flowModelConfigRepo,
		modelconfig.Defaults{
			Generation: cfg.ModelID, //nolint:staticcheck // Seed-only read; the DB is authoritative after boot.
			Quality:    cfg.QualityModelID,
			Planning:   cfg.PlanningModelID,
			Embed:      cfg.EmbedModel,
		},
		vendors.VendorOf,
		tenantTierOf(r),
	)
	logModelCatalog(ctx)
}

// tenantTierOf resolves the tier a model lookup runs under: the tier stamped
// on ctx, else the caller's tenant tier (a rare, LLM-call-time read).
func tenantTierOf(r *repos) func(context.Context) (string, bool) {
	return func(ctx context.Context) (string, bool) {
		if t, ok := tenantctx.TierFrom(ctx); ok {
			return t, true
		}
		tid, ok := tenantctx.From(ctx)
		if !ok {
			return "", false
		}
		t, err := r.tenantRepo.GetByID(ctx, tid)
		if err != nil || t == nil || t.TierID == "" {
			return "", false
		}
		return t.TierID, true
	}
}

// logModelCatalog logs the models this build ships. A short list at boot
// points at a stale binary rather than a configuration problem.
func logModelCatalog(ctx context.Context) {
	var modelIDs []string
	for _, d := range vendors.ByFamily(vendors.FamilyModel) {
		for id := range d.Prices.Models {
			modelIDs = append(modelIDs, d.Name+"/"+id)
		}
	}
	slices.Sort(modelIDs)
	slog.InfoContext(ctx, "model catalog loaded", logging.AttrComponent, "modelconfig",
		"count", len(modelIDs), "models", modelIDs)
}
