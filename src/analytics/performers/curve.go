package performers

import (
	"cmp"
	"slices"
	"strings"
)

// curve holds, per platform, each post's carried-forward metric trajectory, and
// answers "the median metric a platform's posts had at age A".
type curve struct {
	// platform -> postID -> sorted []agePoint (ascending age)
	byPlatform map[string]map[string][]agePoint
}

type agePoint struct {
	age          int
	reach        int
	interactions int
}

func buildCurve(samples []Sample) curve {
	c := curve{byPlatform: map[string]map[string][]agePoint{}}
	for _, s := range samples {
		posts := c.byPlatform[s.Platform]
		if posts == nil {
			posts = map[string][]agePoint{}
			c.byPlatform[s.Platform] = posts
		}
		posts[s.PostID] = append(posts[s.PostID], agePoint{age: s.AgeDay, reach: s.Reach, interactions: s.Interactions})
	}
	for _, posts := range c.byPlatform {
		for id := range posts {
			pts := posts[id]
			slices.SortFunc(pts, func(a, b agePoint) int { return cmp.Compare(a.age, b.age) })
			posts[id] = pts
		}
	}
	return c
}

// metricAt returns a post's carried-forward metric value at ageTarget, and
// whether the post has observed data spanning that age (i.e. a sample at or
// after it, so the carry-forward isn't an extrapolation). Reach/interactions are
// monotonic, so the latest sample at age ≤ target is the value at target.
func metricAt(pts []agePoint, ageTarget int, metric string) (float64, bool) {
	if len(pts) == 0 || pts[0].age > ageTarget {
		return 0, false // post hadn't reached this age in its earliest sample
	}
	if pts[len(pts)-1].age < ageTarget {
		return 0, false // post hasn't lived to this age yet (no extrapolation)
	}
	// last sample with age <= target
	var chosen agePoint
	for _, p := range pts {
		if p.age <= ageTarget {
			chosen = p
		} else {
			break
		}
	}
	return metricOf(chosen, metric), true
}

func metricOf(p agePoint, metric string) float64 {
	switch metric {
	case ByInteractions:
		return float64(p.interactions)
	case ByEngagementRate:
		if p.reach == 0 {
			return 0
		}
		return float64(p.interactions) / float64(p.reach)
	default:
		return float64(p.reach)
	}
}

// multiplier returns candidateValue ÷ expected-at-age for the platform, and
// whether a baseline exists. Returns ok=false when the platform has too few
// posts, or no post lived to (a clamped) target age, or expected is zero.
func (c curve) multiplier(platform string, ageTarget int, metric string, candidate float64) (float64, bool) {
	posts := c.byPlatform[platform]
	if len(posts) < baselineMinPosts {
		return 0, false
	}
	expected, ok := c.expectedAtAge(posts, ageTarget, metric)
	if !ok || expected <= 0 {
		return 0, false
	}
	return candidate / expected, true
}

// expectedAtAge is the median metric across the platform's posts that lived to
// ageTarget. If none did (target beyond all observed ages), it clamps to the
// largest age any post reached (plateau) and retries once.
func (c curve) expectedAtAge(posts map[string][]agePoint, ageTarget int, metric string) (float64, bool) {
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
		// The target may be beyond every post's observed age; clamp to the
		// plateau (largest age any post reached) and retry.
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
	// A median is only meaningful over enough contributors — require at least
	// baselineMinPosts values (not just a non-empty platform), else the multiplier
	// would rest on one or two posts.
	if len(vals) < baselineMinPosts {
		return 0, false
	}
	return median(vals), true
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	slices.Sort(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// --- small text helpers (deterministic copy) ---

var ordinals = []string{"zeroth", "first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth"}

func ordinal(n int) string {
	if n >= 0 && n < len(ordinals) {
		return ordinals[n]
	}
	suffix := "th"
	switch n % 10 {
	case 1:
		if n%100 != 11 {
			suffix = "st"
		}
	case 2:
		if n%100 != 12 {
			suffix = "nd"
		}
	case 3:
		if n%100 != 13 {
			suffix = "rd"
		}
	}
	return itoa(n) + suffix
}

var numberWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}

func numberWord(n int) string {
	if n >= 0 && n < len(numberWords) {
		return numberWords[n]
	}
	return itoa(n)
}

func cap1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// titleCase renders a platform key for display ("linkedin" → "LinkedIn").
func titleCase(platform string) string {
	switch strings.ToLower(platform) {
	case "linkedin":
		return "LinkedIn"
	case "instagram":
		return "Instagram"
	case "facebook":
		return "Facebook"
	case "youtube":
		return "YouTube"
	case "twitter", "x":
		return "X"
	case "threads":
		return "Threads"
	case "tiktok":
		return "TikTok"
	}
	if platform == "" {
		return platform
	}
	return strings.ToUpper(platform[:1]) + platform[1:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
