package post_quality

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The model marks the one phrase of a verdict or an advice a reader must not
// miss as **phrase**; the UI sets it in semibold. Nothing else is markup.
const (
	emphasisMarker = "**"
	// maxEmphasisShare is the largest fraction of the unmarked text the
	// phrase may cover; beyond it the emphasis no longer singles anything out.
	maxEmphasisShare = 0.6
)

// emphasis is a text with its markers removed and the rune range
// [start, end) of its emphasised phrase. start == end means no phrase.
type emphasis struct {
	plain      string
	start, end int
}

// parseEmphasis trims s and splits it into its unmarked text and phrase.
// ok is false when s breaks the contract: more than one pair, an unbalanced
// marker, a blank phrase, a phrase covering maxEmphasisShare or more of the
// text, or a '*' outside the pair. Whitespace just inside the markers moves
// outside the phrase as at most one space.
func parseEmphasis(s string) (e emphasis, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "*") {
		return emphasis{plain: s}, true
	}
	before, rest, found := strings.Cut(s, emphasisMarker)
	if !found {
		return emphasis{}, false
	}
	phrase, after, found := strings.Cut(rest, emphasisMarker)
	if !found || strings.ContainsRune(before+phrase+after, '*') {
		return emphasis{}, false
	}
	trimmed := strings.TrimSpace(phrase)
	if trimmed == "" {
		return emphasis{}, false
	}
	lead, trail, _ := strings.Cut(phrase, trimmed)
	if lead != "" && before != "" && !endsWithSpace(before) {
		before += " "
	}
	if trail != "" && after != "" && !startsWithSpace(after) {
		after = " " + after
	}

	start := utf8.RuneCountInString(before)
	end := start + utf8.RuneCountInString(trimmed)
	plain := before + trimmed + after
	if float64(end-start) >= maxEmphasisShare*float64(utf8.RuneCountInString(plain)) {
		return emphasis{}, false
	}
	return emphasis{plain: plain, start: start, end: end}, true
}

// clipEmphasized clips s to maxChars runes of unmarked text, so the markers
// never count toward the limit. A phrase that survives the clip whole keeps
// its markers; one the clip cuts into is stored plain. When s breaks the
// emphasis contract every '*' is stripped and stripped reports true, so the
// caller can log how often the model misses the format.
func clipEmphasized(s string, maxChars int) (out string, stripped bool) {
	e, ok := parseEmphasis(s)
	if !ok {
		return clipSentences(strings.ReplaceAll(s, "*", ""), maxChars), true
	}
	clipped := clipSentences(e.plain, maxChars)
	if e.start == e.end {
		return clipped, false
	}
	r := []rune(clipped)
	phrase := []rune(e.plain)[e.start:e.end]
	if e.end > len(r) || string(r[e.start:e.end]) != string(phrase) {
		return clipped, false
	}
	return string(r[:e.start]) + emphasisMarker + string(phrase) + emphasisMarker + string(r[e.end:]), false
}

func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

func endsWithSpace(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsSpace(r)
}

// isBlankProse reports whether s has no visible text once markers are gone.
func isBlankProse(s string) bool {
	return strings.TrimFunc(s, func(r rune) bool { return r == '*' || unicode.IsSpace(r) }) == ""
}
