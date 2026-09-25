package content_plan

import (
	"cmp"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
)

// batchSpec is the slot allocator's output for a single batch. Carried into the
// prompt template so the model knows exactly how many posts to produce, of
// which phase and platform mix, and within which date window.
//
// GlobalStartIndex is the slot index of this batch's first post in the
// campaign-wide slot ordering. Combined with the within-batch parse order it
// gives every post a stable global Index that the SSE stream surfaces — under
// parallel batching, posts arrive interleaved by completion order, so the
// stream-arrival index is no longer meaningful as a stable identifier.
type batchSpec struct {
	Index            int
	GlobalStartIndex int
	PostCount        int
	PhaseCounts      []phaseCount
	PlatformCounts   []platformCount
	DateWindow       dateWindow
}

type phaseCount struct {
	PhaseID   string
	PhaseName string
	Sequence  int
	Count     int
}

type platformCount struct {
	PlatformID   string
	PlatformName string
	Count        int
}

// dateWindow is a closed interval [Start, End], both formatted YYYY-MM-DD.
// validateOutput uses string comparison on the same format, so emitting strings
// here keeps the contract end-to-end.
type dateWindow struct {
	Start string
	End   string
}

// planBatches deterministically slices an EstimatedPostCount-sized run into
// per-batch slot specs. Distribution rules (mirroring the system prompt's
// guidance):
//
//   - Posts per phase: even split, remainder to earliest phases (front-loading).
//   - Posts per platform within a phase: even split, remainder to earliest
//     platforms in declared order.
//   - Phase date windows: [StartDate, EndDate] split into N contiguous
//     inclusive ranges, one per phase, in sequence order.
//
// The slot order is (phase ASC by Sequence) → (platform in declared order),
// which means a batch boundary may cross a phase boundary; the batch's
// DateWindow is the union of windows actually touched by its slots.
//
// Returns nil when totalPosts ≤ 0. Caller-side defaults handle K (a non-positive
// maxPostsPerBatch is clamped to totalPosts so we always produce one batch).
func planBatches(
	totalPosts int,
	phases []resolvedPhase,
	platforms []resolvedPlatform,
	startDate, endDate time.Time,
	maxPostsPerBatch int,
) []batchSpec {
	if totalPosts <= 0 || len(phases) == 0 || len(platforms) == 0 {
		return nil
	}
	if maxPostsPerBatch <= 0 {
		maxPostsPerBatch = totalPosts
	}

	// Defensive sort by Sequence — the campaign type repository may or may
	// not order phases for us, and the front-loading rule depends on order.
	sortedPhases := make([]resolvedPhase, len(phases))
	copy(sortedPhases, phases)
	slices.SortStableFunc(sortedPhases, func(a, b resolvedPhase) int {
		return cmp.Compare(a.Sequence, b.Sequence)
	})

	phasePosts := evenSplit(totalPosts, len(sortedPhases))
	phaseWindows := computeDateWindows(startDate, endDate, len(sortedPhases))
	// CON-166: a campaign's manual phase plan pins each phase's window; it is
	// all-or-nothing (the plan is stored whole), so only apply it when every
	// phase carries one.
	if manual := manualWindows(sortedPhases); manual != nil {
		phaseWindows = manual
	}

	type slot struct {
		phaseIdx    int
		platformIdx int
		window      dateWindow
	}
	slots := make([]slot, 0, totalPosts)
	for pi := range sortedPhases {
		if phasePosts[pi] == 0 {
			continue
		}
		platCounts := evenSplit(phasePosts[pi], len(platforms))
		for pli := range platforms {
			for k := 0; k < platCounts[pli]; k++ {
				slots = append(slots, slot{
					phaseIdx:    pi,
					platformIdx: pli,
					window:      phaseWindows[pi],
				})
			}
		}
	}

	// Chunk into batches of K, preserving slot order so phase/date locality
	// is naturally preserved within a batch wherever possible.
	specs := make([]batchSpec, 0, (len(slots)+maxPostsPerBatch-1)/maxPostsPerBatch)
	for i := 0; i < len(slots); i += maxPostsPerBatch {
		end := i + maxPostsPerBatch
		if end > len(slots) {
			end = len(slots)
		}
		chunk := slots[i:end]

		phaseAgg := map[int]int{}
		platAgg := map[int]int{}
		earliest := chunk[0].window.Start
		latest := chunk[0].window.End
		for _, s := range chunk {
			phaseAgg[s.phaseIdx]++
			platAgg[s.platformIdx]++
			if s.window.Start < earliest {
				earliest = s.window.Start
			}
			if s.window.End > latest {
				latest = s.window.End
			}
		}

		// Render phase / platform counts in the same order as the
		// originals so the prompt is stable run-to-run.
		var pcs []phaseCount
		for pi, ph := range sortedPhases {
			if c := phaseAgg[pi]; c > 0 {
				pcs = append(pcs, phaseCount{
					PhaseID:   ph.ID,
					PhaseName: ph.Name,
					Sequence:  ph.Sequence,
					Count:     c,
				})
			}
		}
		var plcs []platformCount
		for pli, pl := range platforms {
			if c := platAgg[pli]; c > 0 {
				plcs = append(plcs, platformCount{
					PlatformID:   pl.ID,
					PlatformName: pl.Name,
					Count:        c,
				})
			}
		}

		specs = append(specs, batchSpec{
			Index:            len(specs),
			GlobalStartIndex: i,
			PostCount:        len(chunk),
			PhaseCounts:      pcs,
			PlatformCounts:   plcs,
			DateWindow:       dateWindow{Start: earliest, End: latest},
		})
	}
	return specs
}

// evenSplit distributes total into n buckets as evenly as possible, giving the
// remainder to the earliest buckets. Returns a slice of length n; n ≤ 0 yields
// nil so callers don't have to special-case empty inputs.
func evenSplit(total, n int) []int {
	if n <= 0 {
		return nil
	}
	out := make([]int, n)
	if total <= 0 {
		return out
	}
	base := total / n
	rem := total % n
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// computeDateWindows partitions the closed inclusive interval [start, end]
// into n contiguous windows in chronological order — the shared
// campaignphase.Split rule (CON-166), so these derived windows are exactly the
// ones GET /api/campaigns/:id/phases reports. When n exceeds the day count
// (rare: e.g. 1-day campaign with multiple phases) all windows collapse to the
// full range — the alternative (zero-day windows) would produce invalid
// publish dates downstream.
func computeDateWindows(start, end time.Time, n int) []dateWindow {
	spans := campaignphase.Split(start, end, n)
	if spans == nil {
		return nil
	}
	out := make([]dateWindow, len(spans))
	for i, sp := range spans {
		out[i] = dateWindow{Start: sp[0].Format(time.DateOnly), End: sp[1].Format(time.DateOnly)}
	}
	return out
}

// manualWindows returns the phases' pinned windows (CON-166 manual plan) in
// order, or nil unless every phase carries one.
func manualWindows(phases []resolvedPhase) []dateWindow {
	out := make([]dateWindow, len(phases))
	for i, ph := range phases {
		if ph.Window == nil {
			return nil
		}
		out[i] = *ph.Window
	}
	return out
}
