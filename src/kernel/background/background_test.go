package background_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/kernel/background"
)

func TestWaitDrainsTasks(t *testing.T) {
	var g background.Group
	var ran atomic.Int32
	for range 5 {
		g.Go("count", func() {
			time.Sleep(10 * time.Millisecond)
			ran.Add(1)
		})
	}
	if err := g.Wait(t.Context()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := ran.Load(); got != 5 {
		t.Fatalf("ran %d tasks, want 5", got)
	}
}

func TestWaitHonoursDeadline(t *testing.T) {
	var g background.Group
	release := make(chan struct{})
	defer close(release)
	g.Go("stuck", func() { <-release })

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait err = %v, want deadline exceeded", err)
	}
}

func TestGoRecoversPanic(t *testing.T) {
	var g background.Group
	g.Go("boom", func() { panic("boom") })
	if err := g.Wait(t.Context()); err != nil {
		t.Fatalf("wait: %v", err)
	}
}
