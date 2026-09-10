package poststat

import (
	"cmp"
	"math"
	"slices"
)

// baselineMinPosts is the per-platform post floor below which a metric's typical
// band is suppressed (mirrors the CON-238 performers curve). A median/quartile
// over fewer than this many posts would rest on one or two observations.
const baselineMinPosts = 3

// Metric keys — the fixed six-card order (CON-250 §3). `views` is not surfaced.
const (
	MetricReach          = "reach"
	MetricImpressions    = "impressions"
	MetricInteractions   = "interactions"
	MetricEngagementRate = "engagement_rate"
	MetricSaves          = "saves"
	MetricClicks         = "clicks"
)

// AgeSample is one historical (platform, post, age-day) observation carrying the
// metric columns the per-metric band needs. Counts are the max seen within that
// age-day (monotonic); engagement rate is derived from interactions ÷ reach.
type AgeSample struct {
	Platform     string
	PostID       string
	AgeDay       int
	Reach        int
	Interactions int
	Impressions  int
	Saves        int
	Clicks       int
}

type bandPoint struct {
	age          int
	reach        int
	interactions int
	impressions  int
	saves        int
	clicks       int
}

// bandCurve answers, per platform, "the p25/p50/p75 metric a platform's posts had
// at age A" — the age-adjusted usual range each card is scored against.
type bandCurve struct {
	byPlatform map[string]map[string][]bandPoint // platform -> postID -> ascending age points
}

func buildBandCurve(samples []AgeSample) bandCurve {
	c := bandCurve{byPlatform: map[string]map[string][]bandPoint{}}
	for _, s := range samples {
		posts := c.byPlatform[s.Platform]
		if posts == nil {
			posts = map[string][]bandPoint{}
			c.byPlatform[s.Platform] = posts
		}
		posts[s.PostID] = append(posts[s.PostID], bandPoint{
			age:          s.AgeDay,
			reach:        s.Reach,
			interactions: s.Interactions,
			impressions:  s.Impressions,
			saves:        s.Saves,
			clicks:       s.Clicks,
		})
	}
	for _, posts := range c.byPlatform {
		for id := range posts {
			pts := posts[id]
			slices.SortFunc(pts, func(a, b bandPoint) int { return cmp.Compare(a.age, b.age) })
			posts[id] = pts
		}
	}
	return c
}

// evaluate returns candidate ÷ p50-at-age and the band label (above/typical/below
// from p25/p75-at-age) for one metric on one platform. ok=false when the platform
// baseline is too thin, no post spans the age, or the median is zero — the card
// then degrades to insufficient_history.
func (c bandCurve) evaluate(platform string, ageTarget int, metric string, candidate float64) (mult float64, band string, ok bool) {
	posts := c.byPlatform[platform]
	if len(posts) < baselineMinPosts {
		return 0, "", false
	}
	vals, ok := c.valuesAtAge(posts, ageTarget, metric)
	if !ok || len(vals) < baselineMinPosts {
		return 0, "", false
	}
	p25 := percentile(vals, 0.25)
	p50 := percentile(vals, 0.50)
	p75 := percentile(vals, 0.75)
	if p50 <= 0 {
		return 0, "", false
	}
	switch {
	case candidate > p75:
		band = "above"
	case candidate < p25:
		band = "below"
	default:
		band = "typical"
	}
	return candidate / p50, band, true
}

// valuesAtAge collects each platform post's carried-forward metric value at
// ageTarget. When the target is beyond every post's observed age it clamps once
// to the plateau (largest age any post reached) — the same guard the CON-238
// curve uses so an old post isn't scored against an empty tail.
func (c bandCurve) valuesAtAge(posts map[string][]bandPoint, ageTarget int, metric string) ([]float64, bool) {
	collect := func(age int) []float64 {
		var vals []float64
		for _, pts := range posts {
			if v, ok := metricAt(pts, age, metric); ok {
				vals = append(vals, v)
			}
		}
		return vals
	}
	vals := collect(ageTarget)
	if len(vals) < baselineMinPosts {
		maxAge := 0
		for _, pts := range posts {
			if len(pts) > 0 && pts[len(pts)-1].age > maxAge {
				maxAge = pts[len(pts)-1].age
			}
		}
		if maxAge > 0 && maxAge < ageTarget {
			vals = collect(maxAge)
		}
	}
	if len(vals) < baselineMinPosts {
		return nil, false
	}
	return vals, true
}

// metricAt returns a post's carried-forward metric at ageTarget, and whether the
// post's observed ages span it (no extrapolation before the first or after the
// last sample). Counters are monotonic, so the latest sample at age ≤ target is
// the value at target.
func metricAt(pts []bandPoint, ageTarget int, metric string) (float64, bool) {
	if len(pts) == 0 || pts[0].age > ageTarget {
		return 0, false
	}
	if pts[len(pts)-1].age < ageTarget {
		return 0, false
	}
	var chosen bandPoint
	for _, p := range pts {
		if p.age <= ageTarget {
			chosen = p
		} else {
			break
		}
	}
	return bandMetricOf(chosen, metric), true
}

func bandMetricOf(p bandPoint, metric string) float64 {
	switch metric {
	case MetricImpressions:
		return float64(p.impressions)
	case MetricInteractions:
		return float64(p.interactions)
	case MetricSaves:
		return float64(p.saves)
	case MetricClicks:
		return float64(p.clicks)
	case MetricEngagementRate:
		if p.reach == 0 {
			return 0
		}
		return float64(p.interactions) / float64(p.reach)
	default:
		return float64(p.reach)
	}
}

// percentile is the linear-interpolation quantile of xs (q ∈ [0,1]).
func percentile(xs []float64, q float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	switch n {
	case 0:
		return 0
	case 1:
		return s[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return s[lo]
	}
	frac := pos - float64(lo)
	return s[lo]*(1-frac) + s[hi]*frac
}
