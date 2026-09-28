// Package poststat assembles the CON-250 per-post statistics drill-down: the
// post header, six metric cards scored against the account's typical post at the
// same age (a per-metric p25/p50/p75 "usual range" band plus an against-typical
// multiplier), the per-metric running-total series since publishing, and a
// deterministic narrative wired to the CON-239 lifespan. Build is a pure function
// over already-fetched data (the post's current metrics + its snapshot
// trajectory + the per-platform baseline samples + the shared lifespan) so it is
// fully unit-testable without a database.
//
// It is the per-post counterpart to the CON-238 ranked board: same age-adjusted
// per-platform baseline, but the band reads p25/p75 so a large multiplier on a
// wide-spread metric (saves) can still read "typical", while a smaller multiplier
// on a tight-spread metric (reach) reads "above". The series spans
// [published_at, now] — there is no window parameter.
package poststat

import (
	"fmt"
	"math"
	"time"

	"github.com/ogen-app/ogen/src/analytics/insights"
)

// metricOrder is the fixed six-card order (CON-250 §3).
var metricOrder = []string{
	MetricReach, MetricImpressions, MetricInteractions,
	MetricEngagementRate, MetricSaves, MetricClicks,
}

const (
	stateReady            = "ready"
	stateAwaitingPlatform = "awaiting_platform"
	baselineInsufficient  = "insufficient_history"
	maxSeriesPoints       = 60
)

// Account is the display block for the post's owning social account.
type Account struct {
	ID          string `json:"id,omitempty"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// CampaignRef is the post's campaign, best-effort (nil when unknown/deleted).
type CampaignRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// PostHeader is the composed "The post" block.
type PostHeader struct {
	PostID          string       `json:"post_id"`
	PublisherPostID string       `json:"publisher_post_id"`
	Title           string       `json:"title"`
	MediaFormat     string       `json:"media_format"`
	Platform        string       `json:"platform"`
	Account         Account      `json:"account"`
	PublishedAt     time.Time    `json:"published_at"`
	AgeHours        int          `json:"age_hours"`
	Campaign        *CampaignRef `json:"campaign"`
	OpenURL         string       `json:"open_url,omitempty"`
}

// Card is one metric tile: the current value, the against-typical multiplier +
// its percentage form, and the usual-range band. AgainstTypical/DeltaPct are nil
// (and Baseline = "insufficient_history") when the platform baseline is too thin.
type Card struct {
	Metric         string   `json:"metric"`
	Value          float64  `json:"value"`
	AgainstTypical *float64 `json:"against_typical"`
	DeltaPct       *float64 `json:"delta_pct"`
	Band           string   `json:"band,omitempty"`
	Baseline       string   `json:"baseline,omitempty"`
}

// SeriesPoint is one point on a metric's running-total-since-publish line.
type SeriesPoint struct {
	AgeHours int     `json:"age_hours"`
	Value    float64 `json:"value"`
}

// Overview is the "Performance overview" section.
type Overview struct {
	State       string             `json:"state"` // ready | awaiting_platform
	WindowLabel string             `json:"window_label"`
	Cards       []Card             `json:"cards"`
	Narrative   []insights.Insight `json:"narrative"`
}

// Response is the assembled per-post payload.
type Response struct {
	UpdatedAt     time.Time                `json:"updated_at"`
	StillCounting bool                     `json:"still_counting"`
	Post          PostHeader               `json:"post"`
	Overview      Overview                 `json:"overview"`
	Series        map[string][]SeriesPoint `json:"series"`
}

// Metrics is a post's metric values at a point in time.
type Metrics struct {
	Reach          int
	Impressions    int
	Interactions   int
	Saves          int
	Clicks         int
	EngagementRate float64
}

// Snapshot is one point on the post's own trajectory (age since publish + the
// metric values at that point).
type Snapshot struct {
	AgeHours int
	Metrics
}

// Lifespan carries the shared CON-239 maturity signals; nil when the workspace
// has too little settled history to have a curve.
type Lifespan struct {
	T50Hours     int
	T95Hours     int
	SettledPosts int
}

// Inputs is everything Build needs, already fetched and tenant-scoped.
type Inputs struct {
	Now       time.Time
	UpdatedAt time.Time
	Header    PostHeader
	Age       time.Duration
	Platform  string
	Current   *Metrics // nil → the platform hasn't reported yet (awaiting_platform)
	Samples   []AgeSample
	Snapshots []Snapshot
	Lifespan  *Lifespan
}

// Build assembles the per-post response.
func Build(in Inputs) Response {
	ageHours := int(in.Age.Hours())
	header := in.Header
	header.AgeHours = ageHours

	resp := Response{
		UpdatedAt: in.UpdatedAt,
		Post:      header,
		Series:    map[string][]SeriesPoint{},
	}

	label := windowLabel(in.Age)

	// still_counting: below the maturity cutoff every figure is a floor. With no
	// lifespan yet, a live post is treated as still counting.
	resp.StillCounting = true
	if in.Lifespan != nil && in.Lifespan.T95Hours > 0 {
		resp.StillCounting = ageHours < in.Lifespan.T95Hours
	}

	// Awaiting-platform: the post is out but the refresh hasn't recorded a row.
	if in.Current == nil {
		resp.Overview = Overview{
			State:       stateAwaitingPlatform,
			WindowLabel: label,
			Cards:       []Card{},
			Narrative:   []insights.Insight{},
		}
		return resp
	}

	curve := buildBandCurve(in.Samples)
	ageDay := ageHours / 24

	cards := make([]Card, 0, len(metricOrder))
	reachBand, reachBandKnown := "", false
	for _, m := range metricOrder {
		val := metricValue(*in.Current, m)
		card := Card{Metric: m, Value: displayValue(m, val)}
		if mult, band, ok := curve.evaluate(in.Platform, ageDay, m, val); ok {
			mm := round2(mult)
			dp := round1((mult - 1) * 100)
			card.AgainstTypical = &mm
			card.DeltaPct = &dp
			card.Band = band
			if m == MetricReach {
				reachBand, reachBandKnown = band, true
			}
		} else {
			card.Baseline = baselineInsufficient
		}
		cards = append(cards, card)
	}

	resp.Overview = Overview{
		State:       stateReady,
		WindowLabel: label,
		Cards:       cards,
		Narrative:   buildNarrative(reachBand, reachBandKnown, label, in.Lifespan),
	}
	resp.Series = buildSeries(in.Snapshots)
	return resp
}

// buildSeries builds a running-total-since-publish line per metric, anchored at
// (0, 0), ending on the latest value. The metrics are cumulative totals already,
// so each snapshot's value is the running total at its age.
func buildSeries(snaps []Snapshot) map[string][]SeriesPoint {
	out := make(map[string][]SeriesPoint, len(metricOrder))
	for _, m := range metricOrder {
		pts := make([]SeriesPoint, 0, len(snaps)+1)
		for _, s := range snaps {
			age := max(s.AgeHours, 0)
			pts = append(pts, SeriesPoint{AgeHours: age, Value: displayValue(m, metricValue(s.Metrics, m))})
		}
		if len(pts) == 0 || pts[0].AgeHours > 0 {
			pts = append([]SeriesPoint{{AgeHours: 0, Value: 0}}, pts...)
		}
		out[m] = downsample(pts, maxSeriesPoints)
	}
	return out
}

// downsample keeps at most max points (the last is always kept), striding
// through the rest. The change-only snapshots are already sparse, so this only
// bites for a very long-lived post.
func downsample(pts []SeriesPoint, max int) []SeriesPoint {
	n := len(pts)
	if n <= max || max < 2 {
		return pts
	}
	budget := max - 1
	step := (n - 1 + budget - 1) / budget // ceil((n-1)/budget)
	if step < 1 {
		step = 1
	}
	out := make([]SeriesPoint, 0, max)
	for i := 0; i < n-1; i += step {
		out = append(out, pts[i])
	}
	return append(out, pts[n-1])
}

func buildNarrative(reachBand string, known bool, label string, life *Lifespan) []insights.Insight {
	out := []insights.Insight{}
	if known {
		var lead string
		switch reachBand {
		case "above":
			lead = fmt.Sprintf("Well ahead of where your posts usually are %s in.", label)
		case "below":
			lead = fmt.Sprintf("Behind where your posts usually are %s in.", label)
		default:
			lead = fmt.Sprintf("About where your posts usually are %s in.", label)
		}
		if life != nil && life.T50Hours > 0 {
			lead += fmt.Sprintf(" Half of what a post of yours earns arrives in its first %s.", hoursPhrase(life.T50Hours))
		}
		out = append(out, insights.Insight{ID: "pace", Severity: insights.Info, Text: lead})
	}
	if life != nil && life.SettledPosts > 0 {
		out = append(out, insights.Insight{
			ID:       "half_life",
			Severity: insights.Note,
			Text:     fmt.Sprintf("Half-life measured across %d finished posts.", life.SettledPosts),
		})
	}
	return out
}

func metricValue(m Metrics, metric string) float64 {
	switch metric {
	case MetricImpressions:
		return float64(m.Impressions)
	case MetricInteractions:
		return float64(m.Interactions)
	case MetricEngagementRate:
		return m.EngagementRate
	case MetricSaves:
		return float64(m.Saves)
	case MetricClicks:
		return float64(m.Clicks)
	default:
		return float64(m.Reach)
	}
}

// displayValue rounds the engagement-rate fraction; counts pass through.
func displayValue(metric string, v float64) float64 {
	if metric == MetricEngagementRate {
		return round4(v)
	}
	return v
}

// windowLabel humanises the post's age for the "over its first …" heading.
func windowLabel(age time.Duration) string {
	if mins := int(age.Minutes()); mins < 60 {
		if mins < 1 {
			mins = 1
		}
		return "first " + plural(mins, "minute")
	}
	if hours := int(age.Hours()); hours < 48 {
		return "first " + plural(hours, "hour")
	}
	return "first " + plural(int(age.Hours())/24, "day")
}

func hoursPhrase(h int) string { return plural(h, "hour") }

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
