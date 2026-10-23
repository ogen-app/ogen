package weeklyreport

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// count formats a whole number for reading: grouped below ten thousand
// ("2,140"), compact above it ("12.9K", "1.2M").
func count(n int) string {
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1_000_000:
		return compact(float64(n)/1_000_000) + "M"
	case abs >= 10_000:
		return compact(float64(n)/1_000) + "K"
	default:
		return grouped(n)
	}
}

// compact renders one decimal, dropping a trailing ".0".
func compact(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(math.Round(f*10)/10, 'f', 1, 64), ".0")
}

func grouped(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "−" + b.String()
	}
	return b.String()
}

// percentDelta is the "↑ 34%" change from prev to cur. ok is false when there
// is nothing to compare against.
func percentDelta(cur, prev float64) (text string, up, ok bool) {
	if prev <= 0 {
		return "", false, false
	}
	pct := math.Round((cur - prev) / prev * 100)
	switch {
	case pct > 0:
		return fmt.Sprintf("↑ %.0f%%", pct), true, true
	case pct < 0:
		return fmt.Sprintf("↓ %.0f%%", -pct), false, true
	default:
		return "No change", false, true
	}
}

// pointsDelta is the "↑ 0.3 pt" change between two percentages.
func pointsDelta(cur, prev float64) (text string, up, ok bool) {
	d := math.Round((cur-prev)*10) / 10
	switch {
	case d > 0:
		return "↑ " + compact(d) + " pt", true, true
	case d < 0:
		return "↓ " + compact(-d) + " pt", false, true
	default:
		return "No change", false, true
	}
}

// signed is "+146" or "−3".
func signed(n int) string {
	if n > 0 {
		return "+" + grouped(n)
	}
	return grouped(n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return grouped(n) + " " + many
}

// rangeLabel is "Oct 13 – 19, 2026", spanning months and years as needed.
func rangeLabel(start, endExcl time.Time) string {
	last := endExcl.AddDate(0, 0, -1)
	switch {
	case start.Year() != last.Year():
		return start.Format("Jan 2, 2006") + " – " + last.Format("Jan 2, 2006")
	case start.Month() != last.Month():
		return start.Format("Jan 2") + " – " + last.Format("Jan 2, 2006")
	default:
		return start.Format("Jan 2") + " – " + last.Format("2, 2006")
	}
}

// postTitle is the post's title, or the start of its content when it has none.
func postTitle(title, content string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = strings.Join(strings.Fields(content), " ")
	}
	if t == "" {
		return "Untitled post"
	}
	return truncate(t, 80)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// platformNames are the display names of the social_accounts platform slugs
// whose capitalisation isn't just the first letter.
var platformNames = map[string]string{
	"linkedin":  "LinkedIn",
	"youtube":   "YouTube",
	"tiktok":    "TikTok",
	"twitter":   "X",
	"x":         "X",
	"instagram": "Instagram",
	"facebook":  "Facebook",
	"threads":   "Threads",
	"pinterest": "Pinterest",
	"bluesky":   "Bluesky",
}

func platformName(slug string) string {
	if n, ok := platformNames[strings.ToLower(slug)]; ok {
		return n
	}
	if slug == "" {
		return ""
	}
	return strings.ToUpper(slug[:1]) + slug[1:]
}

// barHeights scales values to whole pixels under maxPx; a non-zero value is
// never thinner than 3px so it stays visible.
func barHeights(values []int, maxPx int) []int {
	top := 0
	for _, v := range values {
		top = max(top, v)
	}
	out := make([]int, len(values))
	if top == 0 {
		return out
	}
	for i, v := range values {
		if v > 0 {
			out[i] = max(3, int(math.Round(float64(v)/float64(top)*float64(maxPx))))
		}
	}
	return out
}
