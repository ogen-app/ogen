// Package textgist turns model prose into a one-line gist: plain text, one
// line, at most MaxRunes runes.
package textgist

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRunes caps a gist, ellipsis included. The analysis prompt asks for at
// most 140 characters; the cap leaves slack so a slight overrun is kept whole
// instead of cut mid-sentence.
const MaxRunes = 160

var (
	imageRe    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	strongRe   = regexp.MustCompile(`(\*\*|__)(\S(?:.*?\S)?)(\*\*|__)`)
	emStarRe   = regexp.MustCompile(`\*(\S(?:[^*]*?\S)?)\*`)
	emUnderRe  = regexp.MustCompile(`(^|[^\pL\pN_])_(\S(?:[^_]*?\S)?)_($|[^\pL\pN_])`)
	listItemRe = regexp.MustCompile(`^(?:[-*+•]|\d+[.)])\s`)
	// initialsRe matches dotted initials such as "U.S." or "a.m.".
	initialsRe = regexp.MustCompile(`^(?:\pL\.){2,}$`)
)

// abbreviations end in a period without ending the sentence.
var abbreviations = map[string]bool{
	"e.g.": true, "i.e.": true, "etc.": true, "vs.": true, "approx.": true,
	"mr.": true, "mrs.": true, "ms.": true, "dr.": true, "st.": true, "no.": true, "fig.": true,
}

// Clean strips Markdown emphasis, links and code marks, collapses whitespace
// and caps the result at MaxRunes, cutting at a word boundary with an ellipsis.
func Clean(s string) string {
	s = stripMarkdown(s)
	s = strings.Join(strings.Fields(s), " ")
	return capRunes(s, MaxRunes)
}

// FromSummary derives a gist from a Markdown summary: the first sentence of
// the first prose paragraph, skipping headings, lists, quotes, tables and
// bold-only label lines. It returns "" when the summary has no prose.
func FromSummary(md string) string {
	for para := range strings.SplitSeq(strings.ReplaceAll(md, "\r\n", "\n"), "\n\n") {
		var lines []string
		for line := range strings.SplitSeq(para, "\n") {
			if line = strings.TrimSpace(line); line != "" && !isStructural(line) {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			continue
		}
		text := strings.Join(strings.Fields(stripMarkdown(strings.Join(lines, " "))), " ")
		if text == "" {
			continue
		}
		return capRunes(firstSentence(text), MaxRunes)
	}
	return ""
}

// isStructural reports whether a line is Markdown structure rather than prose.
func isStructural(line string) bool {
	switch {
	case strings.HasPrefix(line, "#"), strings.HasPrefix(line, ">"), strings.HasPrefix(line, "|"),
		strings.HasPrefix(line, "```"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "***"):
		return true
	case listItemRe.MatchString(line):
		return true
	}
	// A bold-only line ("**Key takeaways**", "**Overview:**") is a label.
	line = strings.TrimSuffix(line, ":")
	for _, mark := range []string{"**", "__"} {
		if len(line) > 2*len(mark) && strings.HasPrefix(line, mark) && strings.HasSuffix(line, mark) &&
			!strings.Contains(line[len(mark):len(line)-len(mark)], mark) {
			return true
		}
	}
	return false
}

func stripMarkdown(s string) string {
	s = imageRe.ReplaceAllString(s, "$1")
	s = linkRe.ReplaceAllString(s, "$1")
	s = strongRe.ReplaceAllString(s, "$2")
	s = emStarRe.ReplaceAllString(s, "$1")
	s = emUnderRe.ReplaceAllString(s, "$1$2$3")
	return strings.ReplaceAll(s, "`", "")
}

// firstSentence returns text up to and including the first sentence end: a
// terminator followed by a space (or the end), outside known abbreviations.
// Text with no terminator is returned whole.
func firstSentence(text string) string {
	for i, r := range text {
		if !isTerminator(r) {
			continue
		}
		end := i + utf8.RuneLen(r)
		// A run of terminators ("?!", "...") ends together.
		for end < len(text) {
			next, size := utf8.DecodeRuneInString(text[end:])
			if !isTerminator(next) && next != '"' && next != '\'' && next != ')' && next != '”' && next != '’' {
				break
			}
			end += size
		}
		if r == '.' && endsWithAbbreviation(text[:end]) {
			continue
		}
		if end == len(text) || r == '。' || r == '！' || r == '？' {
			return text[:end]
		}
		if next, _ := utf8.DecodeRuneInString(text[end:]); unicode.IsSpace(next) {
			return text[:end]
		}
	}
	return text
}

func isTerminator(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == '。' || r == '！' || r == '？'
}

func endsWithAbbreviation(s string) bool {
	word := s[strings.LastIndexFunc(s, unicode.IsSpace)+1:]
	word = strings.TrimLeft(word, "(\"'“‘")
	return abbreviations[strings.ToLower(word)] || initialsRe.MatchString(word)
}

// capRunes cuts s to at most n runes. A cut lands on the last word boundary
// that fits, drops dangling punctuation and adds an ellipsis.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	cut := string(runes[:n-1])
	// Back off to the last space unless the cut already ends a word.
	if !unicode.IsSpace(runes[n-1]) {
		if i := strings.LastIndexFunc(cut, unicode.IsSpace); i > 0 {
			cut = cut[:i]
		}
	}
	cut = strings.TrimRightFunc(cut, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;:-–—(", r)
	})
	return cut + "…"
}
