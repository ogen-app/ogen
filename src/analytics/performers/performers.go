// Package performers assembles the CON-238 "performers and outliers" board:
// it ranks a window's posts, scores each against the account's typical post for
// its platform and age, splits them into Best/Worst lists, and emits
// deterministic (no-AI) insights. Build is a pure function over already-fetched
// data (in-window candidates + per-(platform,post,age) baseline samples) so it
// is fully unit-testable without a database.
//
// "Against your typical" is age-adjusted per platform: a post's metric divided
// by the median metric its platform's posts had at the same age. That is why a
// young, lower-reach post can outrank an older, higher-reach one. When a
// platform has too little history the multiplier is nil (baseline
// "insufficient_history") and the post is ranked by its raw metric instead.
//
// The baseline curve is computed here at read time from the snapshot history
// (bounded per-tenant volume); the repository method that supplies the samples
// is the seam where a TimescaleDB continuous aggregate can replace the raw scan
// if volume ever grows. See the PRD "Storage foundation update" and §10.
package performers

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/analytics/insights"
)

// Rank bases (the "By …" selector).
const (
	ByAgainstTypical = "against_typical"
	ByReach          = "reach"
	ByEngagementRate = "engagement_rate"
	ByInteractions   = "interactions"
)

// Tunables (constants for now, mirroring the insights thresholds; promote to
// config if operators need to tune them).
const (
	DefaultLimit         = 5
	MaxLimit             = 20
	DefaultFreshnessDays = 3   // reach_still_accruing when age < this
	baselineMinPosts     = 3   // per-platform posts needed for a multiplier
	dirUpper             = 1.2 // >= → "above" (green)
	dirLower             = 0.8 // <= → "below" (red)
	sampleSizeCaveat     = 12  // total posts below this → sample_size insight
	maxInsights          = 3
)

const baselineInsufficient = "insufficient_history"

// ValidBy reports whether a rank basis is supported.
func ValidBy(by string) bool {
	switch by {
	case ByAgainstTypical, ByReach, ByEngagementRate, ByInteractions:
		return true
	}
	return false
}

// Account is the display block for a post's owning social account.
type Account struct {
	ID          string `json:"id,omitempty"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// Candidate is one in-window post with its current metrics + display fields.
type Candidate struct {
	PostID          string
	PublisherPostID string
	Title           string
	Platform        string
	Account         Account
	Reach           int
	Impressions     int
	Likes           int
	Comments        int
	Shares          int
	EngagementRate  float64
	PublishedAt     time.Time
	AgeDays         int
}

// Interactions is the CON-237/238 interactions definition.
func (c Candidate) Interactions() int { return c.Likes + c.Comments + c.Shares }

// Sample is one historical (platform, post, age-day) observation used to build
// the per-platform expected-at-age curve. Reach/Interactions are the max seen
// within that age-day (both monotonic).
type Sample struct {
	Platform     string
	PostID       string
	AgeDay       int
	Reach        int
	Interactions int
}

// Options parameterises the assembly.
type Options struct {
	By            string
	Limit         int
	FreshnessDays int
}

// Metrics is the per-row metric block.
type Metrics struct {
	Impressions    int     `json:"impressions"`
	Likes          int     `json:"likes"`
	Comments       int     `json:"comments"`
	Shares         int     `json:"shares"`
	EngagementRate float64 `json:"engagement_rate"`
}

// Row is one entry in the Best/Worst list.
type Row struct {
	PostID             string    `json:"post_id"`
	PublisherPostID    string    `json:"publisher_post_id"`
	Title              string    `json:"title"`
	Platform           string    `json:"platform"`
	Account            Account   `json:"account"`
	Reach              int       `json:"reach"`
	ReachStillAccruing bool      `json:"reach_still_accruing"`
	PeriodShare        float64   `json:"period_share"`
	Metrics            Metrics   `json:"metrics"`
	AgainstTypical     *float64  `json:"against_typical"`
	Direction          string    `json:"direction,omitempty"`
	Baseline           string    `json:"baseline,omitempty"`
	PublishedAt        time.Time `json:"published_at"`
	AgeDays            int       `json:"age_days"`

	// rawMultiplier is the unrounded against_typical value kept for stable
	// sorting (AgainstTypical exposes the rounded value). Unexported → not
	// serialized.
	rawMultiplier *float64
}

// Result is the assembled board.
type Result struct {
	By         string             `json:"by"`
	TotalPosts int                `json:"total_posts"`
	Best       []Row              `json:"best"`
	Worst      []Row              `json:"worst"`
	Insights   []insights.Insight `json:"insights"`
}

// Build ranks the candidates, scores them against the per-platform curve, splits
// Best/Worst, and adds insights.
func Build(candidates []Candidate, samples []Sample, opts Options) Result {
	by := opts.By
	if !ValidBy(by) {
		by = ByAgainstTypical
	}
	k := opts.Limit
	if k <= 0 {
		k = DefaultLimit
	}
	if k > MaxLimit {
		k = MaxLimit
	}
	fresh := opts.FreshnessDays
	if fresh <= 0 {
		fresh = DefaultFreshnessDays
	}

	metric := multiplierMetric(by)
	curve := buildCurve(samples)

	var totalReach int
	for _, c := range candidates {
		totalReach += c.Reach
	}

	rows := make([]Row, len(candidates))
	for i, c := range candidates {
		row := Row{
			PostID:             c.PostID,
			PublisherPostID:    c.PublisherPostID,
			Title:              c.Title,
			Platform:           c.Platform,
			Account:            c.Account,
			Reach:              c.Reach,
			ReachStillAccruing: c.AgeDays < fresh,
			PeriodShare:        round4(shareOf(c.Reach, totalReach)),
			Metrics: Metrics{
				Impressions:    c.Impressions,
				Likes:          c.Likes,
				Comments:       c.Comments,
				Shares:         c.Shares,
				EngagementRate: c.EngagementRate,
			},
			PublishedAt: c.PublishedAt,
			AgeDays:     c.AgeDays,
		}
		if mult, ok := curve.multiplier(c.Platform, c.AgeDays, metric, candidateMetric(c, metric)); ok {
			raw := mult
			m := round2(mult)
			row.AgainstTypical = &m
			row.rawMultiplier = &raw
			row.Direction = direction(mult)
		} else {
			row.Baseline = baselineInsufficient
		}
		rows[i] = row
	}

	sortRows(rows, by)

	best, worst := partition(rows, k)

	return Result{
		By:         by,
		TotalPosts: len(rows),
		Best:       best,
		Worst:      worst,
		Insights:   buildInsights(rows, len(candidates)),
	}
}

// partition takes the top k and the bottom k (non-overlapping, weakest-first for
// worst) from rows already sorted best-first.
func partition(rows []Row, k int) (best, worst []Row) {
	n := len(rows)
	nb := min(k, n)
	best = append([]Row(nil), rows[:nb]...)
	nw := min(k, n-nb)
	if nw > 0 {
		tail := rows[n-nw:]
		worst = make([]Row, nw)
		for i := range tail { // reverse → weakest first
			worst[i] = tail[nw-1-i]
		}
	}
	return best, worst
}

func sortRows(rows []Row, by string) {
	slices.SortStableFunc(rows, func(x, y Row) int {
		if c := cmp.Compare(rankKey(y, by), rankKey(x, by)); c != 0 { // descending
			return c
		}
		if c := cmp.Compare(y.Reach, x.Reach); c != 0 {
			return c
		}
		return y.PublishedAt.Compare(x.PublishedAt)
	})
}

// rankKey is the sort value for a row under the given basis. A nil multiplier
// sorts to the bottom under against_typical.
func rankKey(r Row, by string) float64 {
	switch by {
	case ByReach:
		return float64(r.Reach)
	case ByEngagementRate:
		return r.Metrics.EngagementRate
	case ByInteractions:
		return float64(r.Metrics.Likes + r.Metrics.Comments + r.Metrics.Shares)
	default: // against_typical — sort on the raw (unrounded) multiplier so the
		// reach tie-breaker only fires on genuine ties, not rounding collisions.
		if r.rawMultiplier == nil {
			return math.Inf(-1)
		}
		return *r.rawMultiplier
	}
}

func multiplierMetric(by string) string {
	if by == ByReach || by == ByEngagementRate || by == ByInteractions {
		return by
	}
	return ByReach // against_typical → reach headline
}

func candidateMetric(c Candidate, metric string) float64 {
	switch metric {
	case ByInteractions:
		return float64(c.Interactions())
	case ByEngagementRate:
		return c.EngagementRate
	default:
		return float64(c.Reach)
	}
}

func direction(mult float64) string {
	switch {
	case mult >= dirUpper:
		return "above"
	case mult <= dirLower:
		return "below"
	default:
		return "typical"
	}
}

func shareOf(reach, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(reach) / float64(total)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- insights ---

func buildInsights(sorted []Row, total int) []insights.Insight {
	out := []insights.Insight{}

	// 1. rank_divergence — best by engagement rate ≠ best by reach.
	byReach := append([]Row(nil), sorted...)
	slices.SortStableFunc(byReach, func(a, b Row) int { return cmp.Compare(b.Reach, a.Reach) })
	byEng := append([]Row(nil), sorted...)
	slices.SortStableFunc(byEng, func(a, b Row) int { return cmp.Compare(b.Metrics.EngagementRate, a.Metrics.EngagementRate) })
	if len(byReach) > 1 && byEng[0].PostID != byReach[0].PostID {
		rank := 1
		for i, r := range byReach {
			if r.PostID == byEng[0].PostID {
				rank = i + 1
				break
			}
		}
		out = append(out, insights.Insight{
			ID:       "rank_divergence",
			Severity: insights.Info,
			Text:     fmt.Sprintf("The best post by engagement rate is only the %s biggest by reach — it reached a smaller, already-following audience, which is why it converted and didn't travel.", ordinal(rank)),
		})
	}

	// 2. sample_size.
	if total > 0 && total < sampleSizeCaveat {
		out = append(out, insights.Insight{
			ID:       "sample_size",
			Severity: insights.Note,
			Text:     fmt.Sprintf("%s posts this period — enough to notice, not enough to call it a rule.", cap1(numberWord(total))),
		})
	}

	// 3. platform_skew — top 3 all one platform.
	topN := min(3, len(sorted))
	if topN >= 2 {
		p := sorted[0].Platform
		same := true
		for _, r := range sorted[:topN] {
			if r.Platform != p {
				same = false
				break
			}
		}
		if same {
			out = append(out, insights.Insight{
				ID:       "platform_skew",
				Severity: insights.Note,
				Text:     fmt.Sprintf("Your top %d all came from %s.", topN, titleCase(p)),
			})
		}
	}

	// 4. spread — wide multiplier dispersion.
	var lo, hi float64
	seen := false
	for _, r := range sorted {
		if r.AgainstTypical == nil {
			continue
		}
		v := *r.AgainstTypical
		if !seen {
			lo, hi, seen = v, v, true
			continue
		}
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if seen && hi >= dirUpper && lo <= dirLower {
		out = append(out, insights.Insight{
			ID:       "spread",
			Severity: insights.Note,
			Text:     fmt.Sprintf("Your posts ranged from %.1f× to %.1f× typical — a high-variance period.", lo, hi),
		})
	}

	// 5. fresh_standout — a still-accruing post already outperforming.
	for _, r := range sorted {
		if r.ReachStillAccruing && r.AgainstTypical != nil && *r.AgainstTypical >= dirUpper {
			out = append(out, insights.Insight{
				ID:       "fresh_standout",
				Severity: insights.Info,
				Text:     fmt.Sprintf("%q is only %d days old and already outperforming your typical.", r.Title, r.AgeDays),
			})
			break
		}
	}

	if len(out) > maxInsights {
		out = out[:maxInsights]
	}
	return out
}
