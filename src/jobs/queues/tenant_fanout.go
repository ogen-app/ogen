package queues

import (
	"context"
	"fmt"
	"runtime/debug"
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
	// tenantStatusWriteTimeout bounds the best-effort health write a sweep makes
	// for a tenant after its own deadline may already have passed.
	tenantStatusWriteTimeout = 10 * time.Second
)

// forEachTenant runs fn for every item, up to tenantSweepParallelism at a time,
// each under its own tenantSweepTimeout derived from ctx, and returns once all
// have finished. Results and errors come back index-aligned with items, so the
// caller folds them without sharing state with fn. A panic in fn is recovered
// and reported as that item's error: the work runs off the River worker's own
// goroutine, where an unrecovered panic would crash the process.
func forEachTenant[T, R any](ctx context.Context, items []T, fn func(ctx context.Context, item T) (R, error)) ([]R, []error) {
	results := make([]R, len(items))
	errs := make([]error, len(items))
	sem := make(chan struct{}, tenantSweepParallelism)
	var wg sync.WaitGroup
	for i, item := range items {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
				}
			}()
			tctx, cancel := context.WithTimeout(ctx, tenantSweepTimeout)
			defer cancel()
			results[i], errs[i] = fn(tctx, item)
		})
	}
	wg.Wait()
	return results, errs
}

// statusWriteContext detaches a per-tenant health write from the tenant's sweep
// deadline (which may be what failed it) while still bounding the write.
func statusWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), tenantStatusWriteTimeout)
}
