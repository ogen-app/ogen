package content_plan

import (
	"testing"
	"time"
)

func TestEvenSplit(t *testing.T) {
	cases := []struct {
		total, n int
		want     []int
	}{
		{10, 3, []int{4, 3, 3}},
		{9, 3, []int{3, 3, 3}},
		{1, 3, []int{1, 0, 0}},
		{0, 3, []int{0, 0, 0}},
		{5, 1, []int{5}},
		{5, 0, nil},
		{0, 0, nil},
	}
	for _, c := range cases {
		got := evenSplit(c.total, c.n)
		if !equalInts(got, c.want) {
			t.Errorf("evenSplit(%d, %d) = %v, want %v", c.total, c.n, got, c.want)
		}
	}
}

func TestComputeDateWindowsContiguous(t *testing.T) {
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-30") // 30 days inclusive

	got := computeDateWindows(start, end, 3)
	want := []dateWindow{
		{Start: "2026-05-01", End: "2026-05-10"},
		{Start: "2026-05-11", End: "2026-05-20"},
		{Start: "2026-05-21", End: "2026-05-30"},
	}
	if !equalWindows(got, want) {
		t.Errorf("computeDateWindows(30 days, 3) = %+v, want %+v", got, want)
	}
}

func TestComputeDateWindowsRemainderFrontLoaded(t *testing.T) {
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-10") // 10 days inclusive

	got := computeDateWindows(start, end, 3)
	// 10/3 = 3 base, remainder 1 → first window gets 4 days, others 3.
	want := []dateWindow{
		{Start: "2026-05-01", End: "2026-05-04"},
		{Start: "2026-05-05", End: "2026-05-07"},
		{Start: "2026-05-08", End: "2026-05-10"},
	}
	if !equalWindows(got, want) {
		t.Errorf("front-loaded 10/3: got %+v want %+v", got, want)
	}
}

func TestComputeDateWindowsCollapsesWhenPhasesExceedDays(t *testing.T) {
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-02") // 2 days, 5 phases

	got := computeDateWindows(start, end, 5)
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i, w := range got {
		if w.Start != "2026-05-01" || w.End != "2026-05-02" {
			t.Errorf("phase %d: got %+v, want full range", i, w)
		}
	}
}

func TestPlanBatchesDistribution(t *testing.T) {
	phases := []resolvedPhase{
		{ID: "ph-1", Name: "Awareness", Sequence: 1},
		{ID: "ph-2", Name: "Consideration", Sequence: 2},
		{ID: "ph-3", Name: "Decision", Sequence: 3},
	}
	platforms := []resolvedPlatform{
		{ID: "linkedin", Name: "LinkedIn"},
		{ID: "x", Name: "X"},
	}
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-30")

	batches := planBatches(120, phases, platforms, start, end, 30)

	if len(batches) != 4 {
		t.Fatalf("want 4 batches for 120/30, got %d", len(batches))
	}

	// Total post count should be exactly 120 across batches.
	total := 0
	for _, b := range batches {
		total += b.PostCount
		// Each batch's phase totals must equal its post count.
		phaseSum := 0
		for _, pc := range b.PhaseCounts {
			phaseSum += pc.Count
		}
		if phaseSum != b.PostCount {
			t.Errorf("batch %d: phase sum %d != PostCount %d", b.Index, phaseSum, b.PostCount)
		}
		platSum := 0
		for _, pc := range b.PlatformCounts {
			platSum += pc.Count
		}
		if platSum != b.PostCount {
			t.Errorf("batch %d: platform sum %d != PostCount %d", b.Index, platSum, b.PostCount)
		}
	}
	if total != 120 {
		t.Errorf("total posts across batches = %d, want 120", total)
	}

	// Per-batch post count: K-sized except possibly the last.
	for i, b := range batches[:len(batches)-1] {
		if b.PostCount != 30 {
			t.Errorf("batch %d: PostCount = %d, want 30", i, b.PostCount)
		}
	}
	// Last batch ≤ 30.
	if batches[len(batches)-1].PostCount > 30 {
		t.Errorf("last batch oversized: %d", batches[len(batches)-1].PostCount)
	}

	// GlobalStartIndex must be cumulative.
	expectedStart := 0
	for _, b := range batches {
		if b.GlobalStartIndex != expectedStart {
			t.Errorf("batch %d: GlobalStartIndex = %d, want %d", b.Index, b.GlobalStartIndex, expectedStart)
		}
		expectedStart += b.PostCount
	}
}

func TestPlanBatchesPhaseFrontLoading(t *testing.T) {
	phases := []resolvedPhase{
		{ID: "ph-1", Name: "Awareness", Sequence: 1},
		{ID: "ph-2", Name: "Consideration", Sequence: 2},
		{ID: "ph-3", Name: "Decision", Sequence: 3},
	}
	platforms := []resolvedPlatform{{ID: "linkedin", Name: "LinkedIn"}}
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-30")

	// 10 posts / 3 phases → [4, 3, 3] (remainder to earliest)
	batches := planBatches(10, phases, platforms, start, end, 100)
	if len(batches) != 1 {
		t.Fatalf("want 1 batch, got %d", len(batches))
	}
	got := map[string]int{}
	for _, pc := range batches[0].PhaseCounts {
		got[pc.PhaseID] = pc.Count
	}
	if got["ph-1"] != 4 || got["ph-2"] != 3 || got["ph-3"] != 3 {
		t.Errorf("phase counts = %v, want ph-1:4 ph-2:3 ph-3:3", got)
	}
}

func TestPlanBatchesUnsortedPhasesAreSortedBySequence(t *testing.T) {
	// Caller passes phases out of order; allocator must front-load by Sequence,
	// not by argument order.
	phases := []resolvedPhase{
		{ID: "ph-3", Name: "Decision", Sequence: 3},
		{ID: "ph-1", Name: "Awareness", Sequence: 1},
		{ID: "ph-2", Name: "Consideration", Sequence: 2},
	}
	platforms := []resolvedPlatform{{ID: "linkedin", Name: "LinkedIn"}}
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-30")

	batches := planBatches(10, phases, platforms, start, end, 100)
	if len(batches) != 1 {
		t.Fatalf("want 1 batch, got %d", len(batches))
	}
	// PhaseCounts emit in Sequence order — ph-1 first, with the +1 remainder.
	pcs := batches[0].PhaseCounts
	if pcs[0].PhaseID != "ph-1" || pcs[0].Count != 4 {
		t.Errorf("first phase = %+v, want ph-1 count=4", pcs[0])
	}
	if pcs[1].PhaseID != "ph-2" || pcs[1].Count != 3 {
		t.Errorf("second phase = %+v, want ph-2 count=3", pcs[1])
	}
	if pcs[2].PhaseID != "ph-3" || pcs[2].Count != 3 {
		t.Errorf("third phase = %+v, want ph-3 count=3", pcs[2])
	}
}

func TestPlanBatchesSkipsPhasesWithZeroPosts(t *testing.T) {
	// 2 posts, 3 phases → [1,1,0]. Only 2 phases should appear in counts.
	phases := []resolvedPhase{
		{ID: "ph-1", Name: "A", Sequence: 1},
		{ID: "ph-2", Name: "B", Sequence: 2},
		{ID: "ph-3", Name: "C", Sequence: 3},
	}
	platforms := []resolvedPlatform{{ID: "linkedin", Name: "LinkedIn"}}
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-30")

	batches := planBatches(2, phases, platforms, start, end, 100)
	if len(batches) != 1 {
		t.Fatalf("want 1 batch, got %d", len(batches))
	}
	if len(batches[0].PhaseCounts) != 2 {
		t.Errorf("PhaseCounts = %+v, want 2 entries", batches[0].PhaseCounts)
	}
}

func TestPlanBatchesEdgeCases(t *testing.T) {
	phases := []resolvedPhase{{ID: "ph-1", Name: "A", Sequence: 1}}
	platforms := []resolvedPlatform{{ID: "linkedin", Name: "LinkedIn"}}
	start := mustDate(t, "2026-05-01")
	end := mustDate(t, "2026-05-30")

	if got := planBatches(0, phases, platforms, start, end, 30); got != nil {
		t.Errorf("totalPosts=0 → %v, want nil", got)
	}
	if got := planBatches(10, nil, platforms, start, end, 30); got != nil {
		t.Errorf("no phases → %v, want nil", got)
	}
	if got := planBatches(10, phases, nil, start, end, 30); got != nil {
		t.Errorf("no platforms → %v, want nil", got)
	}

	// K=0 is treated as "single batch holds everything".
	got := planBatches(7, phases, platforms, start, end, 0)
	if len(got) != 1 || got[0].PostCount != 7 {
		t.Errorf("K=0 fallback = %+v, want one batch of 7", got)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalWindows(a, b []dateWindow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CON-166: a manual phase plan pins each phase's window, but only when every
// phase carries one (the plan is stored whole).
func TestPlanBatchesHonoursManualWindows(t *testing.T) {
	platforms := []resolvedPlatform{{ID: "x", Name: "X"}}
	start, end := mustDate(t, "2026-05-01"), mustDate(t, "2026-05-30")
	pinned := []resolvedPhase{
		{ID: "ph-1", Sequence: 1, Window: &dateWindow{Start: "2026-05-01", End: "2026-05-02"}},
		{ID: "ph-2", Sequence: 2, Window: &dateWindow{Start: "2026-05-03", End: "2026-05-30"}},
	}
	b := planBatches(2, pinned, platforms, start, end, 1)
	if len(b) != 2 || b[0].DateWindow != (dateWindow{Start: "2026-05-01", End: "2026-05-02"}) ||
		b[1].DateWindow != (dateWindow{Start: "2026-05-03", End: "2026-05-30"}) {
		t.Fatalf("batches = %+v, want the pinned windows", b)
	}
	partial := []resolvedPhase{pinned[0], {ID: "ph-2", Sequence: 2}}
	b = planBatches(2, partial, platforms, start, end, 1)
	if b[0].DateWindow != (dateWindow{Start: "2026-05-01", End: "2026-05-15"}) {
		t.Fatalf("partial plan must be ignored, got %+v", b[0].DateWindow)
	}
}
