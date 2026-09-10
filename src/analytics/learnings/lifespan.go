package learnings

import (
	"cmp"
	"slices"
)

const (
	lifespanMinSettled = 8   // settled posts needed for a curve
	settledMinHours    = 96  // a post older than this has "run its course"
	gridCapHours       = 336 // 14 days — cap the curve's right edge
	curveMaxPoints     = 48  // downsample the returned curve to at most this many
)

// CurvePoint is one point on the blended accrual curve.
type CurvePoint struct {
	AgeHours     int     `json:"age_hours"`
	ShareOfFinal float64 `json:"share_of_final"`
}

// Lifespan is the "How long a post lives" section.
type Lifespan struct {
	InsufficientHistory bool         `json:"insufficient_history,omitempty"`
	SettledPosts        int          `json:"settled_posts,omitempty"`
	T50Hours            int          `json:"t50_hours,omitempty"`
	T75Hours            int          `json:"t75_hours,omitempty"`
	T95Hours            int          `json:"t95_hours,omitempty"`
	HorizonHours        int          `json:"horizon_hours,omitempty"`
	Curve               []CurvePoint `json:"curve,omitempty"`
}

type settledPost struct {
	maxAge int
	final  float64
	byHour []float64 // carry-forward reach at each hour 0..maxAge
}

// BuildLifespan exposes the lifespan section builder so other analytics
// surfaces (CON-250's per-post detail) can consume the canonical
// t50/t75/t95/settled_posts maturity signals without recomputing the heatmap or
// pattern sections. It returns {InsufficientHistory:true} below the settled-post
// floor, exactly as the learnings board does.
func BuildLifespan(points []LifespanPoint) *Lifespan { return buildLifespan(points) }

// buildLifespan normalises each settled post's reach trajectory to its final
// value and blends them into one share-of-final curve, then reads t50/t75/t95.
func buildLifespan(points []LifespanPoint) *Lifespan {
	posts := settledPosts(points)
	if len(posts) < lifespanMinSettled {
		return &Lifespan{InsufficientHistory: true}
	}

	gridMax := 0
	for _, p := range posts {
		if p.maxAge > gridMax {
			gridMax = p.maxAge
		}
	}
	if gridMax > gridCapHours {
		gridMax = gridCapHours
	}
	if gridMax == 0 {
		return &Lifespan{InsufficientHistory: true}
	}

	// blended[a] = mean share across posts that lived to age a. Enforce
	// monotonic non-decreasing (per-post share is monotonic; averaging can
	// introduce tiny dips as the contributing set changes).
	blended := make([]float64, gridMax+1)
	for a := 0; a <= gridMax; a++ {
		var sum float64
		var n int
		for _, p := range posts {
			if p.maxAge < a {
				continue
			}
			sum += p.byHour[a] / p.final
			n++
		}
		if n > 0 {
			blended[a] = sum / float64(n)
		}
		if a > 0 && blended[a] < blended[a-1] {
			blended[a] = blended[a-1]
		}
	}

	t50 := firstCross(blended, 0.50)
	t75 := firstCross(blended, 0.75)
	t95 := firstCross(blended, 0.95)
	horizon := firstCross(blended, 0.98)
	if horizon < 0 {
		horizon = gridMax
	}

	return &Lifespan{
		SettledPosts: len(posts),
		T50Hours:     t50,
		T75Hours:     t75,
		T95Hours:     t95,
		HorizonHours: horizon,
		Curve:        downsampleCurve(blended),
	}
}

// settledPosts groups points by post, carries reach forward per hour, and keeps
// only posts old enough that their final value is representative.
func settledPosts(points []LifespanPoint) []settledPost {
	byPost := map[string][]LifespanPoint{}
	for _, p := range points {
		byPost[p.PostID] = append(byPost[p.PostID], p)
	}
	var out []settledPost
	for _, pts := range byPost {
		slices.SortFunc(pts, func(a, b LifespanPoint) int { return cmp.Compare(a.AgeHours, b.AgeHours) })
		maxAge := pts[len(pts)-1].AgeHours
		final := float64(pts[len(pts)-1].Reach)
		if maxAge < settledMinHours || final <= 0 {
			continue
		}
		// The blended curve is only read up to gridCapHours, so cap the grid
		// there rather than allocating out to a very old post's maxAge. final
		// stays the post's true eventual reach (its last sample).
		gridMax := maxAge
		if gridMax > gridCapHours {
			gridMax = gridCapHours
		}
		byHour := make([]float64, gridMax+1)
		pi, last := 0, 0.0
		for a := 0; a <= gridMax; a++ {
			for pi < len(pts) && pts[pi].AgeHours <= a {
				last = float64(pts[pi].Reach)
				pi++
			}
			byHour[a] = last
		}
		out = append(out, settledPost{maxAge: gridMax, final: final, byHour: byHour})
	}
	return out
}

// firstCross returns the smallest age where the blended curve reaches frac, or
// -1 if it never does.
func firstCross(blended []float64, frac float64) int {
	for a, v := range blended {
		if v >= frac {
			return a
		}
	}
	return -1
}

func downsampleCurve(blended []float64) []CurvePoint {
	n := len(blended)
	if n == 0 {
		return nil
	}
	last := n - 1
	// Reserve one slot for the always-included final point so the total never
	// exceeds curveMaxPoints.
	budget := curveMaxPoints - 1
	step := 1
	if budget > 0 && last > budget {
		step = (last + budget - 1) / budget
	}
	out := make([]CurvePoint, 0, curveMaxPoints)
	for a := 0; a < last; a += step {
		out = append(out, CurvePoint{AgeHours: a, ShareOfFinal: round4(blended[a])})
	}
	out = append(out, CurvePoint{AgeHours: last, ShareOfFinal: round4(blended[last])})
	return out
}
