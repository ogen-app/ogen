package handlers

import (
	"context"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// analyticsBaselineTTL is how long a tenant's workspace-wide analytics
// baselines are reused. They aggregate the tenant's whole snapshot history and
// only move when the hourly refresh writes, while every performers, post
// drill-down and lessons view reads them.
const analyticsBaselineTTL = 5 * time.Minute

// analyticsBaselineEntries bounds each baseline cache (one entry per tenant,
// or per tenant and since-day for the lifespan samples).
const analyticsBaselineEntries = 4096

type analyticsBaselines struct {
	reach    *ttlCache[[]repository.ReachAgeSample]
	lifespan *ttlCache[[]repository.LifespanSample]
}

func newAnalyticsBaselines() analyticsBaselines {
	return analyticsBaselines{
		reach:    newTTLCache[[]repository.ReachAgeSample](analyticsBaselineTTL, analyticsBaselineEntries),
		lifespan: newTTLCache[[]repository.LifespanSample](analyticsBaselineTTL, analyticsBaselineEntries),
	}
}

// reachByAgeSamples is repo.ReachByAgeSamples for the ctx tenant, cached. The
// caller gets its own copy, so it may reorder or filter it.
func (h *AnalyticsHandler) reachByAgeSamples(ctx context.Context) ([]repository.ReachAgeSample, error) {
	tid, _ := tenantctx.From(ctx)
	s, err := h.baselines.reach.get(ctx, tid, h.repo.ReachByAgeSamples)
	return slices.Clone(s), err
}

// lifespanSamples is repo.LifespanSamples for the ctx tenant, cached per
// since-day. The caller gets its own copy, so it may reorder or filter it.
func (h *AnalyticsHandler) lifespanSamples(ctx context.Context, since time.Time) ([]repository.LifespanSample, error) {
	tid, _ := tenantctx.From(ctx)
	key := tid + "\x00" + since.UTC().Format(time.DateOnly)
	if since.IsZero() {
		key = tid + "\x00all"
	}
	s, err := h.baselines.lifespan.get(ctx, key, func(ctx context.Context) ([]repository.LifespanSample, error) {
		return h.repo.LifespanSamples(ctx, since)
	})
	return slices.Clone(s), err
}
