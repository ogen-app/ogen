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
	Lift      float64 `json:"lift,omitempty"`
	Trend     float64 `json:"trend,omitempty"`
	Support   int     `json:"support"`
	Detail    string  `json:"detail"`
}

// Patterns is the "What works / What's fading" section.
type Patterns struct {
	InsufficientHistory bool          `json:"insufficient_history,omitempty"`
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

	segs := map[dimSeg]*segAgg{}
	dimSegs := map[string]map[string]bool{}
	recentStart := now.AddDate(0, 0, -trendDays)
	priorStart := now.AddDate(0, 0, -2*trendDays)
	for _, p := range posts {
		v := metricValue(p, metric)
		for _, ds := range segmentsOf(p) {
			a := segs[ds]
			if a == nil {
				a = &segAgg{dim: ds.dim, seg: ds.seg}
				segs[ds] = a
			}
			a.vals = append(a.vals, v)
			switch {
			case !p.PublishedAt.Before(recentStart):
				a.recent = append(a.recent, v)
			case !p.PublishedAt.Before(priorStart):
				a.prior = append(a.prior, v)
			}
			if dimSegs[ds.dim] == nil {
				dimSegs[ds.dim] = map[string]bool{}
			}
			dimSegs[ds.dim][ds.seg] = true
		}
	}

	var works, fading []PatternCard
	for ds, a := range segs {
		if len(dimSegs[ds.dim]) < 2 { // a dimension with one segment has no contrast
			continue
		}
		if len(a.vals) >= minSupport {
			if lift := medianInts(a.vals) / overall; lift >= liftThreshold {
				works = append(works, worksCard(a, metric, lift))
			}
		}
		if len(a.recent) >= minTrendSupport && len(a.prior) >= minTrendSupport {
			if prior := medianInts(a.prior); prior > 0 {
				if trend := medianInts(a.recent) / prior; trend <= fadeThreshold {
					fading = append(fading, fadingCard(a, metric, trend, trendDays))
				}
			}
		}
	}

	// works ranked by lift × log(support); fading by steepest decline. ID breaks
	// ties so capCards picks the same cards deterministically across runs.
	slices.SortFunc(works, func(x, y PatternCard) int {
		ka := x.Lift * math.Log(float64(x.Support+1))
		kb := y.Lift * math.Log(float64(y.Support+1))
		if c := cmp.Compare(kb, ka); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	slices.SortFunc(fading, func(a, b PatternCard) int {
		if c := cmp.Compare(a.Trend, b.Trend); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	return &Patterns{Works: capCards(works), Fading: capCards(fading)}
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

// headline renders a human label for a (dimension, segment).
func headline(dim, seg string) string {
	switch dim {
	case "media_format":
		switch seg {
		case "carousel":
			return "Carousels"
		case "single_image":
			return "Single images"
		case "video":
			return "Video posts"
		case "text_only":
			return "Text-only posts"
		}
	case "content_length":
		switch seg {
		case "long":
			return "Longer posts"
		case "medium":
			return "Medium-length posts"
		case "short":
			return "Short posts"
		}
	case "hashtag_count":
		switch seg {
		case "many":
			return "Posts with lots of hashtags"
		case "few":
			return "Posts with a few hashtags"
		case "none":
			return "Posts with no hashtags"
		}
	case "has_link":
		if seg == "with_link" {
			return "Posts with a link"
		}
		return "Posts without a link"
	case "posting_time":
		if seg == "weekend" {
			return "Weekend posts"
		}
		return "Weekday posts"
	case "platform":
		return titleCasePlatform(seg) + " posts"
	}
	return seg + " " + dim
}
