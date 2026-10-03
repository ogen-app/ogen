// Package overview assembles the CON-237 "what happened over last N days"
// dashboard payload: five KPI cards, per-metric current/previous series, and
// deterministic insights. Build is a pure function over already-fetched data
// (post_analytics_current rows in-window, main-DB published timestamps, and
// follower level totals) so it is fully unit-testable without a database.
//
// The "usual range" baseline band is intentionally not computed yet: it needs a
// long-retention rollup with several prior equal-length windows of history that
// no tenant has accrued (CON-236 collection is recent). Every card therefore
// reports baseline "insufficient_history" and series carry no band. The seam is
// isolated — a baseline provider can populate Card.Baseline + Series.Band later
// without touching the metric math here. See the PRD "Storage foundation
// update" and §6.
package overview

import (
	"math"
	"time"

	"github.com/ogen-app/ogen/src/analytics/insights"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
)

// Baseline label constants (mirror the API contract).
const (
	baselineInsufficient = "insufficient_history"
)

// PostPoint is one published post's latest metrics, keyed by its publish time.
type PostPoint struct {
	At           time.Time
	Reach        int
	Interactions int
}

// FollowerDayTotal is the summed follower level across all accounts on a day.
type FollowerDayTotal struct {
	Date  time.Time
	Total int
}

// Inputs is everything Build needs, already fetched and tenant-scoped.
type Inputs struct {
	Rng, Prev timeframe.Range

	CurPosts, PrevPosts []PostPoint // post_analytics_current rows, published in each window
	CurPublished        []time.Time // main-DB zernio publish timestamps in the current window
	PrevPublished       []time.Time // ... and in the previous window

	// FollowerTotals spans [Prev.From, Rng.To], sorted ascending; FollowersNow is
	// the authoritative current total from the follower summary.
	FollowerTotals []FollowerDayTotal
	FollowersNow   int

	UpdatedAt time.Time
}

// WindowMeta is the resolved window echoed back to the client.
type WindowMeta struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Days        int    `json:"days"`
	Granularity string `json:"granularity"`
}

// Card is one KPI tile.
type Card struct {
	Metric    string    `json:"metric"`
	Label     string    `json:"label"`
	Value     float64   `json:"value"`
	DeltaPct  float64   `json:"delta_pct"`
	Direction string    `json:"direction"`
	Baseline  string    `json:"baseline"`
	Sparkline []float64 `json:"sparkline"`
}

// BandPoint is one bucket's usual-range band (unused until baselines exist).
type BandPoint struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// Series is a metric's aligned current/previous/band arrays over the buckets.
type Series struct {
	Buckets  []string    `json:"buckets"`
	Current  []float64   `json:"current"`
	Previous []float64   `json:"previous"`
	Band     []BandPoint `json:"band,omitempty"`
}

// Response is the assembled dashboard payload.
type Response struct {
	Window    WindowMeta         `json:"window"`
	UpdatedAt time.Time          `json:"updated_at"`
	Cards     []Card             `json:"cards"`
	Series    map[string]Series  `json:"series"`
	Insights  []insights.Insight `json:"insights"`
	// FollowersScope is "workspace" when the read is narrowed to a campaign:
	// followers belong to accounts, not campaigns, so that card stays
	// workspace-wide. Absent on an un-narrowed read.
	FollowersScope string `json:"followers_scope,omitempty"`
}

type metricSpec struct {
	key   string
	label string
}

var metricOrder = []metricSpec{
	{"reach", "Cumulative reach"},
	{"interactions", "Cumulative interactions"},
	{"engagement_rate", "Daily engagement rate"},
	{"followers", "Current followers"},
	{"posts_published", "Posts published"},
}

// Build computes the full overview payload.
func Build(in Inputs) Response {
	curBuckets := in.Rng.Buckets()
	labels := in.Rng.Labels()

	// --- reach & interactions (flows: cumulative sum by publish bucket) ---
	reachCur := cumulative(in.Rng, pointsBy(in.CurPosts, func(p PostPoint) float64 { return float64(p.Reach) }))
	reachPrev := cumulative(in.Prev, pointsBy(in.PrevPosts, func(p PostPoint) float64 { return float64(p.Reach) }))
	intCur := cumulative(in.Rng, pointsBy(in.CurPosts, func(p PostPoint) float64 { return float64(p.Interactions) }))
	intPrev := cumulative(in.Prev, pointsBy(in.PrevPosts, func(p PostPoint) float64 { return float64(p.Interactions) }))

	// per-bucket (non-cumulative) reach/interactions drive the engagement-rate
	// series and the peak-bucket insight.
	reachPer := perBucket(in.Rng, pointsBy(in.CurPosts, func(p PostPoint) float64 { return float64(p.Reach) }))
	intPer := perBucket(in.Rng, pointsBy(in.CurPosts, func(p PostPoint) float64 { return float64(p.Interactions) }))

	// --- engagement rate (ratio: interactions / reach) ---
	engCur := ratioSeries(intPer, reachPer)
	engPrev := ratioSeries(
		perBucket(in.Prev, pointsBy(in.PrevPosts, func(p PostPoint) float64 { return float64(p.Interactions) })),
		perBucket(in.Prev, pointsBy(in.PrevPosts, func(p PostPoint) float64 { return float64(p.Reach) })),
	)
	engRateCur := safeRatio(sum(intPer), sum(reachPer))
	engRatePrev := safeRatio(lastNonCumSum(in.PrevPosts, func(p PostPoint) int { return p.Interactions }),
		lastNonCumSum(in.PrevPosts, func(p PostPoint) int { return p.Reach }))

	// --- posts published (flow: cumulative count) ---
	postsCur := cumulative(in.Rng, timesToPoints(in.CurPublished))
	postsPrev := cumulative(in.Prev, timesToPoints(in.PrevPublished))

	// --- followers (level: carry-forward daily total) ---
	folCur := followerLevels(in.Rng, in.FollowerTotals)
	folPrev := followerLevels(in.Prev, in.FollowerTotals)
	// FollowersNow from the summary is authoritative — trust it even when 0, so a
	// genuine zero total isn't overwritten by a stale carry-forward level (the
	// value also feeds the insights' FollowersCur).
	followersNow := float64(in.FollowersNow)
	followersStart := firstNonZero(folCur)

	series := map[string]Series{
		"reach":           {Buckets: labels, Current: reachCur, Previous: alignLen(reachPrev, len(curBuckets))},
		"interactions":    {Buckets: labels, Current: intCur, Previous: alignLen(intPrev, len(curBuckets))},
		"engagement_rate": {Buckets: labels, Current: engCur, Previous: alignLen(engPrev, len(curBuckets))},
		"followers":       {Buckets: labels, Current: folCur, Previous: alignLen(folPrev, len(curBuckets))},
		"posts_published": {Buckets: labels, Current: postsCur, Previous: alignLen(postsPrev, len(curBuckets))},
	}

	values := map[string]float64{
		"reach":           last(reachCur),
		"interactions":    last(intCur),
		"engagement_rate": engRateCur,
		"followers":       followersNow,
		"posts_published": last(postsCur),
	}
	deltas := map[string]float64{
		"reach":           pct(last(reachCur), last(reachPrev)),
		"interactions":    pct(last(intCur), last(intPrev)),
		"engagement_rate": pct(engRateCur, engRatePrev),
		"followers":       pct(followersNow, followersStart),
		"posts_published": pct(last(postsCur), last(postsPrev)),
	}

	cards := make([]Card, 0, len(metricOrder))
	for _, m := range metricOrder {
		s := series[m.key]
		cards = append(cards, Card{
			Metric:    m.key,
			Label:     m.label,
			Value:     round2(values[m.key]),
			DeltaPct:  deltas[m.key],
			Direction: direction(deltas[m.key]),
			Baseline:  baselineInsufficient,
			Sparkline: s.Current,
		})
	}

	// --- insights ---
	peakLabel, peakShare := peakBucket(reachPer, labels)
	ins := insights.Build(insights.Input{
		ReachCur:         intFromFloat(last(reachCur)),
		ReachPrev:        intFromFloat(last(reachPrev)),
		InteractionsCur:  intFromFloat(last(intCur)),
		InteractionsPrev: intFromFloat(last(intPrev)),
		EngRateCur:       engRateCur,
		EngRatePrev:      engRatePrev,
		FollowersCur:     intFromFloat(followersNow),
		FollowersPrev:    intFromFloat(last(folPrev)),
		PostsCur:         intFromFloat(last(postsCur)),
		PostsPrev:        intFromFloat(last(postsPrev)),
		PeakBucketLabel:  peakLabel,
		PeakBucketShare:  peakShare,
		FollowerStreak:   trailingGrowth(folCur),
	})
	if ins == nil {
		ins = []insights.Insight{}
	}

	return Response{
		Window: WindowMeta{
			From:        in.Rng.FromISO(),
			To:          in.Rng.ToISO(),
			Days:        in.Rng.Days,
			Granularity: string(in.Rng.Granularity),
		},
		UpdatedAt: in.UpdatedAt,
		Cards:     cards,
		Series:    series,
		Insights:  ins,
	}
}

// --- series helpers ---

type valPoint struct {
	At  time.Time
	Val float64
}

func pointsBy(pts []PostPoint, f func(PostPoint) float64) []valPoint {
	out := make([]valPoint, len(pts))
	for i, p := range pts {
		out[i] = valPoint{At: p.At, Val: f(p)}
	}
	return out
}

func timesToPoints(ts []time.Time) []valPoint {
	out := make([]valPoint, len(ts))
	for i, t := range ts {
		out[i] = valPoint{At: t, Val: 1}
	}
	return out
}

func perBucket(r timeframe.Range, pts []valPoint) []float64 {
	out := make([]float64, len(r.Buckets()))
	for _, p := range pts {
		if idx := r.BucketIndex(p.At); idx >= 0 && idx < len(out) {
			out[idx] += p.Val
		}
	}
	return out
}

func cumulative(r timeframe.Range, pts []valPoint) []float64 {
	out := perBucket(r, pts)
	for i := 1; i < len(out); i++ {
		out[i] += out[i-1]
	}
	return out
}

func ratioSeries(num, den []float64) []float64 {
	out := make([]float64, len(num))
	for i := range num {
		out[i] = round4(safeRatio(int(num[i]), int(den[i])))
	}
	return out
}

// followerLevels builds a carry-forward daily follower total series over r from
// the (whole-span) totals, so gaps between snapshots hold the last known value.
func followerLevels(r timeframe.Range, totals []FollowerDayTotal) []float64 {
	buckets := r.Buckets()
	out := make([]float64, len(buckets))
	ti := 0
	var last float64
	// advance to the last total strictly before the window (carry-in level)
	for ti < len(totals) && totals[ti].Date.Before(r.From) {
		last = float64(totals[ti].Total)
		ti++
	}
	for i, b := range buckets {
		end := r.BucketEnd(b)
		for ti < len(totals) && totals[ti].Date.Before(end) {
			last = float64(totals[ti].Total)
			ti++
		}
		out[i] = last
	}
	return out
}

// --- scalar helpers ---

func sum(xs []float64) int {
	var s float64
	for _, x := range xs {
		s += x
	}
	return int(s)
}

func lastNonCumSum(pts []PostPoint, f func(PostPoint) int) int {
	var s int
	for _, p := range pts {
		s += f(p)
	}
	return s
}

func safeRatio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func last(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return xs[len(xs)-1]
}

func firstNonZero(xs []float64) float64 {
	for _, x := range xs {
		if x != 0 {
			return x
		}
	}
	return 0
}

// alignLen pads or truncates s to n elements so previous-window series line up
// with the current window's bucket axis by offset.
func alignLen(s []float64, n int) []float64 {
	if len(s) == n {
		return s
	}
	out := make([]float64, n)
	copy(out, s)
	return out
}

func pct(cur, prev float64) float64 {
	if prev == 0 {
		return 0
	}
	return round1((cur - prev) / prev * 100)
}

func direction(deltaPct float64) string {
	switch {
	case deltaPct > 0.5:
		return "up"
	case deltaPct < -0.5:
		return "down"
	default:
		return "flat"
	}
}

func peakBucket(reachPer []float64, labels []string) (string, float64) {
	total := 0.0
	maxIdx, maxVal := -1, 0.0
	for i, v := range reachPer {
		total += v
		if v > maxVal {
			maxVal, maxIdx = v, i
		}
	}
	if maxIdx < 0 || total == 0 {
		return "", 0
	}
	return labels[maxIdx], maxVal / total
}

// trailingGrowth counts the consecutive buckets of strict follower growth at the
// end of the level series.
func trailingGrowth(levels []float64) int {
	n := 0
	for i := len(levels) - 1; i > 0; i-- {
		if levels[i] > levels[i-1] {
			n++
		} else {
			break
		}
	}
	return n
}

func intFromFloat(f float64) int { return int(math.Round(f)) }

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
