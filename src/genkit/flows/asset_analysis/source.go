package asset_analysis

import (
	"strings"
	"unicode/utf8"
)

// estimateTokens is a rough count for text without a source-reported one:
// about four bytes of English per token.
func estimateTokens(s string) int {
	return max(1, utf8.RuneCountInString(s)/4)
}

func segmentTokens(s Segment) int {
	if s.Tokens > 0 {
		return s.Tokens
	}
	return estimateTokens(s.Text)
}

// budgeted keeps the segments with words, in order, until limit tokens are
// used. It reports the tokens kept and whether anything was cut.
func budgeted(segments []Segment, limit int) (kept []Segment, tokens int, truncated bool) {
	for _, s := range segments {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		n := segmentTokens(s)
		if tokens+n > limit {
			return kept, tokens, true
		}
		kept = append(kept, s)
		tokens += n
	}
	return kept, tokens, false
}

// groups splits segments into consecutive runs of at most size tokens. A
// segment larger than size gets a run of its own.
func groups(segments []Segment, size int) [][]Segment {
	var out [][]Segment
	var cur []Segment
	used := 0
	for _, s := range segments {
		n := segmentTokens(s)
		if len(cur) > 0 && used+n > size {
			out = append(out, cur)
			cur, used = nil, 0
		}
		cur = append(cur, s)
		used += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// labelSet is every label the model may cite.
func labelSet(segments []Segment) map[string]bool {
	set := make(map[string]bool, len(segments))
	for _, s := range segments {
		if s.Label != "" {
			set[s.Label] = true
		}
	}
	return set
}

// wordCount counts whitespace-separated words across the segments.
func wordCount(segments []Segment) int {
	n := 0
	for _, s := range segments {
		n += len(strings.Fields(s.Text))
	}
	return n
}
