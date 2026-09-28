package learnings

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"
)

const (
	minSupport      = 8   // posts in a segment for it to count (works)
	minTrendSupport = 4   // posts per window for a trend (fading)
	liftThreshold   = 1.3 // segment median ≥ this × overall → "works"
	fadeThreshold   = 0.8 // recent ÷ prior ≤ this → "fading"
	maxCards        = 3
)

// PatternCard is one "works"/"fading" finding.
type PatternCard struct {
	ID        string  `json:"id"`
	Dimension string  `json:"dimension"`
	Segment   string  `json:"segment"`
	Headline  string  `json:"headline"`
	Metric    string  `json:"metric"`
	Lift      float64 `json:"lift,omitzero"`
	Trend     float64 `json:"trend,omitzero"`
	Support   int     `json:"support"`
	Detail    string  `json:"detail"`
}

// Patterns is the "What works / What's fading" section.
type Patterns struct {
	InsufficientHistory bool          `json:"insufficient_history,omitzero"`
	Works               []PatternCard `json:"works,omitempty"`
	Fading              []PatternCard `json:"fading,omitempty"`
}

type dimSeg struct{ dim, seg string }

type segAgg struct {
	dim, seg      string
	vals          []int
	recent, prior []int
}

// buildPatterns mines structural segments and flags those notably above the
// overall median ("works") or declining over the trend window ("fading").
func buildPatterns(posts []PostFact, metric string, now time.Time, trendDays int) *Patterns {
	if len(posts) < minSupport {
		return &Patterns{InsufficientHistory: true}
	}
	all := make([]int, len(posts))
	for i, p := range posts {
		all[i] = metricValue(p, metric)
	}
	overall := medianInts(all)
	if overall <= 0 {
		return &Patterns{InsufficientHistory: true}
	}

	segs, segsPerDim := aggregateSegments(posts, metric, now, trendDays)
	works, fading := selectCards(segs, segsPerDim, overall, metric, trendDays)

	// works ranked by lift × log(support); fading by steepest decline. ID breaks
	// ties so capCards picks the same cards deterministically across runs.
	slices.SortFunc(works, func(x, y PatternCard) int {
		ka := x.Lift * math.Log(float64(x.Support+1))
		kb := y.Lift * math.Log(float64(y.Support+1))
		return cmp.Or(cmp.Compare(kb, ka), cmp.Compare(x.ID, y.ID))
	})
	slices.SortFunc(fading, func(a, b PatternCard) int {
		return cmp.Or(cmp.Compare(a.Trend, b.Trend), cmp.Compare(a.ID, b.ID))
	})

	return &Patterns{Works: capCards(works), Fading: capCards(fading)}
}

// aggregateSegments buckets each post's metric value into every segment it
// belongs to, splitting the trend windows into recent and prior. It also
// returns how many distinct segments each dimension has.
func aggregateSegments(posts []PostFact, metric string, now time.Time, trendDays int) (map[dimSeg]*segAgg, map[string]int) {
	segs := map[dimSeg]*segAgg{}
	segsPerDim := map[string]int{}
	recentStart := now.AddDate(0, 0, -trendDays)
	priorStart := now.AddDate(0, 0, -2*trendDays)
	for _, p := range posts {
		v := metricValue(p, metric)
		for _, ds := range segmentsOf(p) {
			a := segs[ds]
			if a == nil {
				a = &segAgg{dim: ds.dim, seg: ds.seg}
				segs[ds] = a
				segsPerDim[ds.dim]++
			}
			a.vals = append(a.vals, v)
			switch {
			case !p.PublishedAt.Before(recentStart):
				a.recent = append(a.recent, v)
			case !p.PublishedAt.Before(priorStart):
				a.prior = append(a.prior, v)
			}
		}
	}
	return segs, segsPerDim
}

// selectCards turns qualifying segments into unsorted works and fading
// cards. A dimension with a single segment has no contrast and is skipped.
func selectCards(segs map[dimSeg]*segAgg, segsPerDim map[string]int, overall float64, metric string, trendDays int) (works, fading []PatternCard) {
	for ds, a := range segs {
		if segsPerDim[ds.dim] < 2 {
			continue
		}
		if lift, ok := segmentLift(a, overall); ok {
			works = append(works, worksCard(a, metric, lift))
		}
		if trend, ok := segmentTrend(a); ok {
			fading = append(fading, fadingCard(a, metric, trend, trendDays))
		}
	}
	return works, fading
}

// segmentLift reports the segment's median relative to overall when it has
// enough support and clears liftThreshold.
func segmentLift(a *segAgg, overall float64) (float64, bool) {
	if len(a.vals) < minSupport {
		return 0, false
	}
	lift := medianInts(a.vals) / overall
	return lift, lift >= liftThreshold
}

// segmentTrend reports recent ÷ prior median when both windows have enough
// support and the ratio is at or below fadeThreshold.
func segmentTrend(a *segAgg) (float64, bool) {
	if len(a.recent) < minTrendSupport || len(a.prior) < minTrendSupport {
		return 0, false
	}
	prior := medianInts(a.prior)
	if prior <= 0 {
		return 0, false
	}
	trend := medianInts(a.recent) / prior
	return trend, trend <= fadeThreshold
}

func worksCard(a *segAgg, metric string, lift float64) PatternCard {
	return PatternCard{
		ID:        a.dim + ":" + a.seg,
		Dimension: a.dim,
		Segment:   a.seg,
		Headline:  headline(a.dim, a.seg),
		Metric:    metric,
		Lift:      round2(lift),
		Support:   len(a.vals),
		Detail:    worksDetail(metric, lift),
	}
}

func fadingCard(a *segAgg, metric string, trend float64, trendDays int) PatternCard {
	return PatternCard{
		ID:        a.dim + ":" + a.seg,
		Dimension: a.dim,
		Segment:   a.seg,
		Headline:  headline(a.dim, a.seg),
		Metric:    metric,
		Trend:     round2(trend),
		Support:   len(a.recent) + len(a.prior),
		Detail:    fmt.Sprintf("Down to %s of its usual %s over the last %d days.", fractionWord(trend), metricNoun(metric), trendDays),
	}
}

func worksDetail(metric string, lift float64) string {
	if lift < 2 {
		return fmt.Sprintf("Roughly %.0f%% more %s than a typical post.", (lift-1)*100, metricNoun(metric))
	}
	return fmt.Sprintf("About %.1f× your median %s.", lift, metricNoun(metric))
}

func capCards(c []PatternCard) []PatternCard {
	if len(c) > maxCards {
		return c[:maxCards]
	}
	return c
}

// segmentsOf derives every structural (dimension, segment) a post belongs to.
func segmentsOf(f PostFact) []dimSeg {
	out := make([]dimSeg, 0, 6)

	switch {
	case f.MediaCount == 0:
		out = append(out, dimSeg{"media_format", "text_only"})
	case f.IsVideo:
		out = append(out, dimSeg{"media_format", "video"})
	case f.MediaCount > 1:
		out = append(out, dimSeg{"media_format", "carousel"})
	default:
		out = append(out, dimSeg{"media_format", "single_image"})
	}

	switch {
	case f.ContentLength <= 120:
		out = append(out, dimSeg{"content_length", "short"})
	case f.ContentLength <= 400:
		out = append(out, dimSeg{"content_length", "medium"})
	default:
		out = append(out, dimSeg{"content_length", "long"})
	}

	switch {
	case f.HashtagCount == 0:
		out = append(out, dimSeg{"hashtag_count", "none"})
	case f.HashtagCount <= 3:
		out = append(out, dimSeg{"hashtag_count", "few"})
	default:
		out = append(out, dimSeg{"hashtag_count", "many"})
	}

	if f.HasLink {
		out = append(out, dimSeg{"has_link", "with_link"})
	} else {
		out = append(out, dimSeg{"has_link", "no_link"})
	}

	if wd := f.PublishedAt.UTC().Weekday(); wd == time.Saturday || wd == time.Sunday {
		out = append(out, dimSeg{"posting_time", "weekend"})
	} else {
		out = append(out, dimSeg{"posting_time", "weekday"})
	}

	if f.Platform != "" {
		out = append(out, dimSeg{"platform", f.Platform})
	}
	return out
}

// headlines labels the fixed (dimension, segment) pairs.
var headlines = map[dimSeg]string{
	{"media_format", "carousel"}:     "Carousels",
	{"media_format", "single_image"}: "Single images",
	{"media_format", "video"}:        "Video posts",
	{"media_format", "text_only"}:    "Text-only posts",
	{"content_length", "long"}:       "Longer posts",
	{"content_length", "medium"}:     "Medium-length posts",
	{"content_length", "short"}:      "Short posts",
	{"hashtag_count", "many"}:        "Posts with lots of hashtags",
	{"hashtag_count", "few"}:         "Posts with a few hashtags",
	{"hashtag_count", "none"}:        "Posts with no hashtags",
	{"has_link", "with_link"}:        "Posts with a link",
	{"posting_time", "weekend"}:      "Weekend posts",
}

// headline renders a human label for a (dimension, segment).
func headline(dim, seg string) string {
	if h, ok := headlines[dimSeg{dim, seg}]; ok {
		return h
	}
	switch dim {
	case "has_link":
		return "Posts without a link"
	case "posting_time":
		return "Weekday posts"
	case "platform":
		return titleCasePlatform(seg) + " posts"
	}
	return seg + " " + dim
}
