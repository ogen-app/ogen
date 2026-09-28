// Package background tracks best-effort goroutines spawned off a request path,
// so shutdown can wait for them instead of killing them mid-write.
package background

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Group is a set of tracked goroutines. The zero value is ready to use.
type Group struct {
	wg sync.WaitGroup
}

// Go runs fn in a tracked goroutine. A panic in fn is logged and recovered: a
// best-effort side task must never take the process down.
func (g *Group) Go(name string, fn func()) {
	g.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background task panicked",
					logging.AttrComponent, "background", "task", name,
					logging.AttrError, fmt.Errorf("%v", r), "stack", string(debug.Stack()))
			}
		}()
		fn()
	})
}

// Wait blocks until every tracked goroutine has returned or ctx ends,
// returning ctx's error in the latter case.
func (g *Group) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
