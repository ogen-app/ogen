package platforms

import (
	"strings"
	"unicode"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SplitThread turns a single authored body into the ordered thread segments that
// the rest of the pipeline publishes (CON-284 R2). It is the whole point of the
// R2 revision: `posts.content` is the canonical draft (exactly what the author
// typed) and `thread_segments` is DERIVED from it here on every write, inverting
// R1 (which mirrored content from segments[0]).
//
// Two modes, chosen by the content itself:
//
//   - Manual — if the body contains any thematic-break delimiter line (three or
//     more of "-", "*" or "_" alone on a line — see isRuleLine), split on those
//     lines. The author is in explicit control of every break; perSegmentLimit is
//     ignored.
//   - Auto — with no delimiter present, greedily pack the body into segments each
//     at most perSegmentLimit characters long, breaking on the best available
//     boundary in priority order: paragraph (blank line) > line (single newline) >
//     sentence > word > hard cut. Length is measured after flattening Markdown
//     (VisibleLen), since the per-message limit governs what publishes, not the
//     syntax around it.
//
// perSegmentLimit is the platform's per-segment ceiling
// (TextConstraints.ContentLimitFor("thread") — X 280 / Threads 500). A limit <= 0
// means "unknown" (e.g. a draft saved before a platform is picked): auto mode then
// returns the whole body as one segment, and manual splitting still works. Every
// returned segment is trimmed and non-empty; a body that yields fewer than two
// segments is left as-is for validateThread to reject (a one-message body is not a
// thread).
func SplitThread(content string, perSegmentLimit int) models.ThreadSegments {
	// Normalise CRLF so delimiter detection and per-line joins don't leave stray
	// carriage returns inside a segment.
	content = strings.ReplaceAll(content, "\r\n", "\n")

	parts := splitManual(content)
	if parts == nil {
		parts = autoSplit(content, perSegmentLimit)
	}

	segs := make(models.ThreadSegments, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			segs = append(segs, models.ThreadSegment{Content: p})
		}
	}
	return segs
}

// isRuleLine reports whether a line (already trimmed of surrounding whitespace) is
// a thread delimiter: a Markdown thematic break — three or more of the SAME marker
// character ('-', '*' or '_'), alone on the line, with optional spaces between
// them (CommonMark §4.1). This is the horizontal rule the BlockNote composer
// inserts between messages; its serialiser emits "***" by default (mdast-util-to-
// markdown's rule:'*', ×3), so matching only hyphens — as R2 first shipped — split
// on a delimiter the editor never actually writes, and every authored break fell
// through into one long message (CON-284). Mixed markers ("-*-") and any line with
// other characters ("**bold**") are not rules.
func isRuleLine(trimmed string) bool {
	var marker rune
	count := 0
	for _, r := range trimmed {
		switch r {
		case '-', '*', '_':
			if count == 0 {
				marker = r
			} else if r != marker {
				return false
			}
			count++
		case ' ', '\t':
			// interior spaces are allowed between markers ("- - -")
		default:
			return false
		}
	}
	return count >= 3
}

// splitManual splits on delimiter lines, or returns nil when the body has none
// (signalling the caller to fall back to auto mode). Chunks are returned untrimmed
// and possibly empty; SplitThread trims and drops empties, so consecutive /
// leading / trailing delimiters never produce empty segments.
func splitManual(content string) []string {
	lines := strings.Split(content, "\n")
	if !anyRuleLine(lines) {
		return nil
	}
	var parts []string
	var cur []string
	for _, ln := range lines {
		if isRuleLine(strings.TrimSpace(ln)) {
			parts = append(parts, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, ln)
	}
	return append(parts, strings.Join(cur, "\n"))
}

func anyRuleLine(lines []string) bool {
	for _, ln := range lines {
		if isRuleLine(strings.TrimSpace(ln)) {
			return true
		}
	}
	return false
}

// autoSplit packs a delimiter-free body into <=limit-rune segments, preferring
// coarse boundaries and only descending to finer ones (line, sentence, word, hard
// cut) for a unit that is itself too large. Greedy combination keeps each segment
// as full as the next unit allows. Lines sit between paragraphs and sentences so
// an over-limit paragraph is first broken on its own single newlines — preserving
// that structure — before falling back to sentence and word boundaries.
func autoSplit(content string, limit int) []string {
	text := strings.TrimSpace(content)
	if text == "" {
		return nil
	}
	if limit <= 0 || VisibleLen(text) <= limit {
		return []string{text}
	}
	return packUnits(splitParagraphs(text), limit, "\n\n", func(para string) []string {
		return packUnits(splitLines(para), limit, "\n", func(line string) []string {
			return packUnits(splitSentences(line), limit, " ", func(sentence string) []string {
				return packUnits(splitWords(sentence), limit, " ", func(word string) []string {
					return hardCut(word, limit)
				})
			})
		})
	})
}

// packUnits greedily joins units (with joiner) into <=limit-rune chunks. A unit
// that does not fit the current chunk flushes it; a unit that alone exceeds limit
// is broken down by overflow, whose leading pieces are flushed and whose last
// piece stays open so the following unit can pack onto it.
func packUnits(units []string, limit int, joiner string, overflow func(string) []string) []string {
	var out []string
	var cur string
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, u := range units {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		candidate := u
		if cur != "" {
			candidate = cur + joiner + u
		}
		if VisibleLen(candidate) <= limit {
			cur = candidate
			continue
		}
		flush()
		if VisibleLen(u) <= limit {
			cur = u
			continue
		}
		pieces := overflow(u)
		for i, p := range pieces {
			if i == len(pieces)-1 {
				cur = p
			} else {
				out = append(out, p)
			}
		}
	}
	flush()
	return out
}

// splitLines breaks a paragraph into its individual lines (single-newline
// separated) — the boundary between paragraph and sentence granularity, so an
// over-limit paragraph keeps its line structure where the lines still fit.
func splitLines(text string) []string { return strings.Split(text, "\n") }

// splitParagraphs groups the body into paragraphs separated by blank (whitespace-
// only) lines, preserving single newlines inside a paragraph.
func splitParagraphs(text string) []string {
	lines := strings.Split(text, "\n")
	var paras []string
	var cur []string
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			if len(cur) > 0 {
				paras = append(paras, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		paras = append(paras, strings.Join(cur, "\n"))
	}
	return paras
}

// splitSentences breaks text after a run of terminators (. ! ?) that is followed
// by whitespace or end-of-text, keeping the terminator with its sentence.
func splitSentences(text string) []string {
	runes := []rune(text)
	var out []string
	start := 0
	for i := 0; i < len(runes); i++ {
		if !isTerminator(runes[i]) {
			continue
		}
		j := i
		for j+1 < len(runes) && isTerminator(runes[j+1]) {
			j++
		}
		if j+1 >= len(runes) || unicode.IsSpace(runes[j+1]) {
			if s := strings.TrimSpace(string(runes[start : j+1])); s != "" {
				out = append(out, s)
			}
			start = j + 1
		}
		i = j
	}
	if start < len(runes) {
		if s := strings.TrimSpace(string(runes[start:])); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isTerminator(r rune) bool { return r == '.' || r == '!' || r == '?' }

// splitWords splits on any run of whitespace.
func splitWords(text string) []string { return strings.Fields(text) }

// hardCut slices a single over-limit unit (a word with no internal boundary, e.g.
// a very long URL) into limit-rune pieces — the last resort so no segment can ever
// exceed the platform ceiling.
func hardCut(text string, limit int) []string {
	runes := []rune(text)
	var out []string
	for len(runes) > limit {
		out = append(out, string(runes[:limit]))
		runes = runes[limit:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}
