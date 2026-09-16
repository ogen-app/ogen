package platforms

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FlattenSocialText turns the editor's Markdown into the plain text a social
// platform actually shows. The post editor stores Markdown (BlockNote's
// blocksToMarkdownLossy), but none of the networks we publish to render it —
// captions are plain text with newlines. posts.content stays Markdown all the way
// to Zernio, which flattens it at the publish boundary (CON-126); the author's
// source is never rewritten.
//
// We need the flattened form for measurement, not egress: a thread's per-message
// character budget (X 280 / Threads 500) and the auto-split both have to count
// what publishes, not the Markdown syntax around it — otherwise `**bold**` spends
// 8 of 280 characters instead of 4, and the auto-split can cut through a markup
// run (CON-284 R2 defect). This is the Go source-of-truth port of the front end's
// ui/src/lib/socialText.ts markdownToSocialText; the two must stay in agreement so
// the counter the server enforces is the number the composer shows.
func FlattenSocialText(markdown string) string {
	if markdown == "" {
		return ""
	}

	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	inFence := false

	for _, line := range lines {
		// Fenced code: keep the contents, drop the fences. Nothing inside is
		// Markdown, so it passes through untouched.
		if reFence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}

		// Thematic breaks have no plain-text equivalent worth showing.
		if isRuleLine(strings.TrimSpace(line)) {
			continue
		}

		line = reHeading.ReplaceAllString(line, "")       // heading marker
		line = reBlockquote.ReplaceAllString(line, "")    // blockquote marker
		line = reBullet.ReplaceAllString(line, "$1• ")    // bullet list
		line = reOrdered.ReplaceAllString(line, "$1$2. ") // ordered list

		out = append(out, inlineToText(line))
	}

	// Blank runs collapse to one empty line: Markdown needs the double newline
	// between blocks, a feed post does not.
	return strings.TrimSpace(reBlankRuns.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
}

// VisibleLen is the rune length of the text as it will publish — the flattened
// length that the platform character limit actually governs. Every place that
// counts a thread against the limit (the auto-split, the publish-gate per-segment
// check, the preview endpoint's char_count) routes through here, so none of them
// can disagree with the composer about the same message.
func VisibleLen(s string) int { return utf8.RuneCountInString(FlattenSocialText(s)) }

// maskChar is the NUL sentinel that wraps a masked escape while inline rules run;
// NUL never appears in real post copy, so it can't collide with authored text.
const maskChar = "\x00"

var (
	reFence      = regexp.MustCompile("^\\s*(```|~~~)")
	reHeading    = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	reBlockquote = regexp.MustCompile(`^\s*>\s?`)
	reBullet     = regexp.MustCompile(`^(\s*)[-*+]\s+`)
	reOrdered    = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+`)
	reBlankRuns  = regexp.MustCompile(`\n{3,}`)

	reImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	reLink     = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	reAutolink = regexp.MustCompile(`<((?:https?|mailto):[^>]+)>`)
	reCode     = regexp.MustCompile("`([^`]+)`")

	// Fixed-delimiter emphasis. RE2 has no backreferences, so the front end's
	// (\*\*\*|___)…\1 pattern is expanded into explicit same-marker pairs. Order
	// matters: strip the longest markers first so ** inside *** is already gone.
	reBoldItalicStar  = regexp.MustCompile(`\*\*\*(.+?)\*\*\*`)
	reBoldItalicUnder = regexp.MustCompile(`___(.+?)___`)
	reBoldStar        = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reBoldUnder       = regexp.MustCompile(`__(.+?)__`)
	reStrike          = regexp.MustCompile(`~~(.+?)~~`)

	// Escaped punctuation is masked before any other rule runs (so `\*` is never
	// read as emphasis) and restored last; masking after would eat the escaped
	// character along with the marker.
	reEscaped = regexp.MustCompile("\\\\([!-/:-@\\[-`{-~])")
	reMasked  = regexp.MustCompile(maskChar + `(\d+)` + maskChar)
)

// inlineToText strips inline Markdown from a single line, leaving the text a
// caption will display. Mirrors socialText.ts inlineToText rule-for-rule.
func inlineToText(input string) string {
	s := reEscaped.ReplaceAllStringFunc(input, func(m string) string {
		// m is `\X`; X is the escaped ASCII-punctuation byte.
		return maskChar + strconv.Itoa(int(m[1])) + maskChar
	})

	// Images: the alt text is all a caption can carry.
	s = reImage.ReplaceAllString(s, "$1")

	// Links: keep the URL — it stays clickable once published and counts toward
	// the character limit, so hiding it would understate the length.
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		g := reLink.FindStringSubmatch(m)
		text, url := g[1], g[2]
		if text == "" || text == url {
			return url
		}
		return text + " (" + url + ")"
	})
	s = reAutolink.ReplaceAllString(s, "$1")

	s = reCode.ReplaceAllString(s, "$1")            // inline code
	s = reBoldItalicStar.ReplaceAllString(s, "$1")  // bold italic ***
	s = reBoldItalicUnder.ReplaceAllString(s, "$1") // bold italic ___
	s = reBoldStar.ReplaceAllString(s, "$1")        // bold **
	s = reBoldUnder.ReplaceAllString(s, "$1")       // bold __
	s = stripItalic(s, '*')                         // italic *
	s = stripItalic(s, '_')                         // italic _
	s = reStrike.ReplaceAllString(s, "$1")          // strikethrough

	return reMasked.ReplaceAllStringFunc(s, func(m string) string {
		code, _ := strconv.Atoi(m[len(maskChar) : len(m)-len(maskChar)])
		return string(rune(code))
	})
}

// stripItalic removes single-marker emphasis (* or _), reproducing the JS
// lookaround rules that RE2 cannot express: an opening marker is not preceded by
// the same marker or a word char and not followed by whitespace; the closing
// marker's preceding char is not whitespace, and — for '*' — it is not followed
// by another '*' (that would be bold, already stripped), while for '_' it is not
// followed by a word char (so intra_word_underscores survive). Emphasis content
// carries no marker, so the first marker after the opener is the only candidate
// close.
func stripItalic(s string, marker rune) string {
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); {
		if runes[i] == marker && canOpenEmphasis(runes, i, marker) {
			if j := findCloseEmphasis(runes, i, marker); j > 0 {
				b.WriteString(string(runes[i+1 : j]))
				i = j + 1
				continue
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

func canOpenEmphasis(runes []rune, i int, marker rune) bool {
	if i > 0 {
		if prev := runes[i-1]; prev == marker || isWordRune(prev) {
			return false
		}
	}
	next, ok := runeAt(runes, i+1)
	return ok && !isSpaceRune(next) && next != marker
}

func findCloseEmphasis(runes []rune, open int, marker rune) int {
	for j := open + 1; j < len(runes); j++ {
		if runes[j] != marker {
			continue
		}
		if j == open+1 || isSpaceRune(runes[j-1]) { // empty or trailing space
			return -1
		}
		if follow, ok := runeAt(runes, j+1); ok {
			if marker == '*' && follow == '*' {
				return -1
			}
			if marker == '_' && isWordRune(follow) {
				return -1
			}
		}
		return j
	}
	return -1
}

func runeAt(runes []rune, i int) (rune, bool) {
	if i < 0 || i >= len(runes) {
		return 0, false
	}
	return runes[i], true
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// isWordRune matches JavaScript's ASCII-only \w.
func isWordRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}
