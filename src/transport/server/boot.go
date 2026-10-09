package server

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

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
	// Code defaults seed any missing global-default row; Harbor owns every
	// assignment after that. The embed slot is seeded with EMBED_MODEL, the
	// model the embedder actually runs.
	seeds := modelconfig.SeedDefaults
	seeds.Embed = cfg.EmbedModel
	modelconfig.Init(ctx, r.flowModelConfigRepo, seeds, vendors.VendorOf, tenantTierOf(r))
	logModelCatalog(ctx)
}

// tierCacheTTL bounds how stale a tenant's tier may be when picking a model;
// it matches the model-config snapshot's own refresh interval.
const tierCacheTTL = time.Minute

// tenantTierOf resolves the tier a model lookup runs under: the tier stamped
// on ctx, else the caller's tenant tier. Every model call resolves it (a run
// of parallel quality checks or batched drafts makes several), so tenant tiers
// are cached for tierCacheTTL rather than read each time.
func tenantTierOf(r *repos) func(context.Context) (string, bool) {
	type entry struct {
		tier    string
		expires time.Time
	}
	var (
		mu    sync.Mutex
		tiers = map[string]entry{}
	)
	return func(ctx context.Context) (string, bool) {
		if t, ok := tenantctx.TierFrom(ctx); ok {
			return t, true
		}
		tid, ok := tenantctx.From(ctx)
		if !ok {
			return "", false
		}
		now := time.Now()
		mu.Lock()
		e, hit := tiers[tid]
		mu.Unlock()
		if hit && now.Before(e.expires) {
			return e.tier, e.tier != ""
		}
		t, err := r.tenantRepo.GetByID(ctx, tid)
		if err != nil || t == nil {
			return "", false
		}
		mu.Lock()
		tiers[tid] = entry{tier: t.TierID, expires: now.Add(tierCacheTTL)}
		mu.Unlock()
		return t.TierID, t.TierID != ""
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
