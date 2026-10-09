package queues

import (
	"context"
	"sync"
	"time"
)

const (
	// tenantSweepParallelism caps how many tenants a periodic Zernio sweep
	// works on at once.
	tenantSweepParallelism = 4
	// tenantSweepTimeout bounds one tenant's part of a sweep, so a slow tenant
	// cannot starve the rest.
	tenantSweepTimeout = 60 * time.Second
	// tenantSweepJobTimeout is the River deadline for a whole fanned-out sweep.
	tenantSweepJobTimeout = 10 * time.Minute
)

// forEachTenant runs fn for every item, up to tenantSweepParallelism at a time,
// each under its own tenantSweepTimeout derived from ctx. It returns once all
// have finished. fn must be safe to run concurrently; results are collected by
// the caller (under its own lock).
func forEachTenant[T any](ctx context.Context, items []T, fn func(ctx context.Context, item T)) {
	sem := make(chan struct{}, tenantSweepParallelism)
	var wg sync.WaitGroup
	for _, item := range items {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			tctx, cancel := context.WithTimeout(ctx, tenantSweepTimeout)
			defer cancel()
			fn(tctx, item)
		})
	}
	wg.Wait()
}
