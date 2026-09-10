package content_plan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBatch builds a batchSpec with predictable PostCount + GlobalStartIndex
// for fan-out tests. Phases / platforms aren't relevant to runBatchesParallel
// itself — that helper is generator-agnostic.
func fakeBatch(idx, start, count int) batchSpec {
	return batchSpec{
		Index:            idx,
		GlobalStartIndex: start,
		PostCount:        count,
		DateWindow:       dateWindow{Start: "2026-05-01", End: "2026-05-30"},
	}
}

func TestRunBatchesParallelSuccessAggregatesInOrder(t *testing.T) {
	batches := []batchSpec{
		fakeBatch(0, 0, 3),
		fakeBatch(1, 3, 3),
		fakeBatch(2, 6, 2),
	}

	// Stagger completion: batch 1 finishes first, batch 0 second, batch 2 last
	// — exercises the "must aggregate in batch order, not completion order"
	// invariant.
	delays := []time.Duration{30 * time.Millisecond, 5 * time.Millisecond, 60 * time.Millisecond}
	gen := func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]DraftPost, error) {
		time.Sleep(delays[spec.Index])
		out := make([]DraftPost, spec.PostCount)
		for i := range out {
			out[i] = DraftPost{Title: fmt.Sprintf("b%d-p%d", spec.Index, i)}
			if emit != nil {
				emit(SSEEventPost, PostEventPayload{Post: out[i], Index: spec.GlobalStartIndex + i})
			}
		}
		return out, nil
	}

	var emitted []PostEventPayload
	var emitMu sync.Mutex
	emit := func(_ SSEEventKind, data any) {
		p := data.(PostEventPayload)
		emitMu.Lock()
		emitted = append(emitted, p)
		emitMu.Unlock()
	}

	posts, warnings, err := runBatchesParallel(t.Context(), batches, 5, gen, emit)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}

	// Aggregated posts must be in batch order regardless of completion order.
	wantTitles := []string{"b0-p0", "b0-p1", "b0-p2", "b1-p0", "b1-p1", "b1-p2", "b2-p0", "b2-p1"}
	if len(posts) != len(wantTitles) {
		t.Fatalf("posts len = %d, want %d", len(posts), len(wantTitles))
	}
	for i, w := range wantTitles {
		if posts[i].Title != w {
			t.Errorf("posts[%d] = %q, want %q", i, posts[i].Title, w)
		}
	}

	// Emitted posts may arrive in any order, but their Index values must
	// cover [0..7] exactly once each.
	if len(emitted) != 8 {
		t.Fatalf("emitted len = %d, want 8", len(emitted))
	}
	idxs := make([]int, len(emitted))
	for i, p := range emitted {
		idxs[i] = p.Index
	}
	slices.Sort(idxs)
	for i, v := range idxs {
		if v != i {
			t.Errorf("emitted index set has gap: idxs[%d] = %d", i, v)
		}
	}
}

func TestRunBatchesParallelPartialSuccess(t *testing.T) {
	batches := []batchSpec{
		fakeBatch(0, 0, 3),
		fakeBatch(1, 3, 3), // this one fails
		fakeBatch(2, 6, 2),
	}

	gen := func(ctx context.Context, spec batchSpec, _ OnEventFunc) ([]DraftPost, error) {
		if spec.Index == 1 {
			return nil, errors.New("simulated batch 1 failure")
		}
		out := make([]DraftPost, spec.PostCount)
		for i := range out {
			out[i] = DraftPost{Title: fmt.Sprintf("b%d-p%d", spec.Index, i)}
		}
		return out, nil
	}

	posts, warnings, err := runBatchesParallel(t.Context(), batches, 5, gen, nil)
	if err != nil {
		t.Fatalf("partial-success run should not error: %v", err)
	}
	if len(posts) != 5 { // batches 0 and 2: 3 + 2
		t.Errorf("posts len = %d, want 5", len(posts))
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one failure warning", warnings)
	}
	if !contains(warnings[0], "batch 2/3") || !contains(warnings[0], "slots 3-5") {
		t.Errorf("warning text = %q, want batch index + slot range", warnings[0])
	}
}

func TestRunBatchesParallelAllFailedReturnsAIError(t *testing.T) {
	batches := []batchSpec{
		fakeBatch(0, 0, 2),
		fakeBatch(1, 2, 2),
	}

	gen := func(_ context.Context, _ batchSpec, _ OnEventFunc) ([]DraftPost, error) {
		return nil, errors.New("model unavailable")
	}

	posts, warnings, err := runBatchesParallel(t.Context(), batches, 5, gen, nil)
	if err == nil {
		t.Fatal("expected error when all batches fail")
	}
	if _, ok := errors.AsType[*AIError](err); !ok {
		t.Errorf("err = %T %v, want *AIError", err, err)
	}
	if posts != nil {
		t.Errorf("posts = %v, want nil", posts)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want 2", warnings)
	}
}

func TestRunBatchesParallelMaxParallelCapHonoured(t *testing.T) {
	batches := make([]batchSpec, 10)
	for i := range batches {
		batches[i] = fakeBatch(i, i*2, 2)
	}

	const cap = 3
	var inFlight atomic.Int32
	var maxObserved atomic.Int32

	gen := func(_ context.Context, spec batchSpec, _ OnEventFunc) ([]DraftPost, error) {
		current := inFlight.Add(1)
		for {
			prev := maxObserved.Load()
			if current <= prev || maxObserved.CompareAndSwap(prev, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		out := make([]DraftPost, spec.PostCount)
		return out, nil
	}

	_, _, err := runBatchesParallel(t.Context(), batches, cap, gen, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := maxObserved.Load(); got > cap {
		t.Errorf("max parallel observed = %d, want ≤ %d", got, cap)
	}
}

// Concurrency contract: emit is single-writer. With many goroutines hammering
// emit we must never observe an interleaved write — verify by serialising
// through the safeEmit wrapper installed in runBatchesParallel.
func TestRunBatchesParallelEmitIsSerialised(t *testing.T) {
	batches := make([]batchSpec, 8)
	for i := range batches {
		batches[i] = fakeBatch(i, i*5, 5)
	}

	gen := func(_ context.Context, spec batchSpec, emit OnEventFunc) ([]DraftPost, error) {
		out := make([]DraftPost, spec.PostCount)
		for i := range out {
			emit(SSEEventPost, PostEventPayload{Post: DraftPost{}, Index: spec.GlobalStartIndex + i})
			out[i] = DraftPost{}
		}
		return out, nil
	}

	// onEvent that asserts non-reentrancy via a counter — increment on entry,
	// require seeing 1, decrement on exit. Any concurrent call would push
	// the counter ≥ 2 and fail the test.
	var inside atomic.Int32
	emit := func(_ SSEEventKind, _ any) {
		if got := inside.Add(1); got != 1 {
			t.Errorf("concurrent emit: inside = %d", got)
		}
		// Hold briefly to amplify any race.
		time.Sleep(time.Microsecond)
		inside.Add(-1)
	}

	if _, _, err := runBatchesParallel(t.Context(), batches, 8, gen, emit); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
