package weeklyreport

import (
	"fmt"
	"math"
	"strings"
)

// reachSwing is the reach change, in percent, that the headline calls out.
const reachSwing = 10

// headline is the report's one-sentence summary, built from fixed rules so the
// same week always reads the same way. The first rule that matches wins.
func headline(f Facts) string {
	n := len(f.Published)
	if n == 0 {
		return "Nothing went out last week."
	}
	posts := plural(n, "post went", "posts went") + " out"
	if pct, ok := reachChange(f); ok {
		switch {
		case pct >= reachSwing:
			return fmt.Sprintf("%s and reach is up %d%%.", posts, pct)
		case pct <= -reachSwing:
			return fmt.Sprintf("%s; reach is down %d%%.", posts, -pct)
		}
	}
	if prev := f.Counts.PrevPublished; prev > 0 && n*2 <= prev {
		return "A quiet week: " + posts + "."
	}
	return posts + " last week."
}

func reachChange(f Facts) (int, bool) {
	a := f.Analytics
	if a == nil || a.PrevReach <= 0 {
		return 0, false
	}
	return int(math.Round(float64(a.Reach-a.PrevReach) / float64(a.PrevReach) * 100)), true
}

// lede follows the headline: the best post, then what didn't go out, or what's
// lined up when nothing failed.
func lede(f Facts) string {
	var parts []string
	if a := f.Analytics; a != nil && len(a.Top) > 0 && a.Top[0].Reach > 0 {
		best := a.Top[0]
		s := fmt.Sprintf("Your best post reached %s people", count(best.Reach))
		if best.Multiple > 0 {
			s += fmt.Sprintf(", %s× what a %s post of yours usually gets at that age",
				compact(math.Round(best.Multiple*10)/10), platformName(best.Platform))
		}
		parts = append(parts, s+".")
	}
	switch failed := f.Counts.Failed; {
	case failed > 0:
		parts = append(parts, plural(failed, "post", "posts")+" didn't go out.")
	case f.Counts.ScheduledNext > 0 && len(f.Published) > 0:
		parts = append(parts, fmt.Sprintf("Nothing failed, and %s lined up for next week.",
			plural(f.Counts.ScheduledNext, "post is", "posts are")))
	case f.Counts.ScheduledNext > 0:
		parts = append(parts, fmt.Sprintf("%s lined up for next week.",
			capitalize(plural(f.Counts.ScheduledNext, "post is", "posts are"))))
	}
	return strings.Join(parts, " ")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
