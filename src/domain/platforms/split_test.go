package platforms

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
)

func segContents(segs models.ThreadSegments) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.Content
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSplitThread_ManualDelimiter(t *testing.T) {
	got := segContents(SplitThread("Root msg\n\n---\n\nReply msg", 280))
	want := []string{"Root msg", "Reply msg"}
	if !equalStrings(got, want) {
		t.Errorf("manual split: want %q, got %q", want, got)
	}
}

func TestSplitThread_ManualIgnoresLimit(t *testing.T) {
	// Manual delimiters win even when a segment exceeds the limit — the author
	// is in control; validateThread reports the over-limit segment later.
	long := strings.Repeat("a", 400)
	got := segContents(SplitThread(long+"\n---\nshort", 280))
	want := []string{long, "short"}
	if !equalStrings(got, want) {
		t.Errorf("manual over-limit: want 2 segments (first len %d), got %q", len(long), got)
	}
}

func TestSplitThread_ManualEdges(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"consecutive delimiters": {"A\n---\n---\nB", []string{"A", "B"}},
		"leading delimiter":      {"---\nA\n---\nB", []string{"A", "B"}},
		"trailing delimiter":     {"A\n---\nB\n---", []string{"A", "B"}},
		"five hyphens":           {"A\n-----\nB", []string{"A", "B"}},
		"spaced delimiter":       {"A\n  ---  \nB", []string{"A", "B"}},
		"only one non-empty":     {"A\n---\n   ", []string{"A"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := segContents(SplitThread(tc.in, 280))
			if !equalStrings(got, tc.want) {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestSplitThread_ManualMultilineSegment(t *testing.T) {
	got := segContents(SplitThread("line1\nline2\n---\nlineB", 280))
	want := []string{"line1\nline2", "lineB"}
	if !equalStrings(got, want) {
		t.Errorf("multiline segment: want %q, got %q", want, got)
	}
}

func TestSplitThread_ManualCRLF(t *testing.T) {
	got := segContents(SplitThread("A\r\n---\r\nB", 280))
	want := []string{"A", "B"}
	if !equalStrings(got, want) {
		t.Errorf("CRLF split: want %q, got %q", want, got)
	}
}

func TestSplitThread_AutoUnderLimitSingleSegment(t *testing.T) {
	// No delimiter and within the limit → one segment. validateThread will
	// reject the single-message "thread" downstream; that's intended.
	got := segContents(SplitThread("just a short post", 280))
	if len(got) != 1 || got[0] != "just a short post" {
		t.Errorf("under-limit: want single segment, got %q", got)
	}
}

func TestSplitThread_AutoParagraphPacking(t *testing.T) {
	// Two 4-char paragraphs pack together under a 10-char limit; the third
	// paragraph would overflow, so it starts a new segment.
	got := segContents(SplitThread("aaaa\n\nbbbb\n\ncccc", 10))
	want := []string{"aaaa\n\nbbbb", "cccc"}
	if !equalStrings(got, want) {
		t.Errorf("paragraph packing: want %q, got %q", want, got)
	}
}

func TestSplitThread_AutoSentenceSplit(t *testing.T) {
	got := segContents(SplitThread("This is one. This is two. This is three.", 20))
	want := []string{"This is one.", "This is two.", "This is three."}
	if !equalStrings(got, want) {
		t.Errorf("sentence split: want %q, got %q", want, got)
	}
}

func TestSplitThread_AutoWordAndHardCut(t *testing.T) {
	// A single word longer than the limit is hard-cut into limit-sized pieces.
	got := segContents(SplitThread("abcdefghij", 5))
	want := []string{"abcde", "fghij"}
	if !equalStrings(got, want) {
		t.Errorf("hard cut: want %q, got %q", want, got)
	}

	// Words pack greedily on spaces up to the limit.
	got = segContents(SplitThread("aa bb cc dd", 5))
	want = []string{"aa bb", "cc dd"}
	if !equalStrings(got, want) {
		t.Errorf("word packing: want %q, got %q", want, got)
	}
}

func TestSplitThread_AutoExactAtLimit(t *testing.T) {
	got := segContents(SplitThread("aaaaa\n\nbbbbb", 5))
	want := []string{"aaaaa", "bbbbb"}
	if !equalStrings(got, want) {
		t.Errorf("exact-at-limit: want %q, got %q", want, got)
	}
}

func TestSplitThread_UnknownLimitSingleSegment(t *testing.T) {
	// limit <= 0 (draft without a platform) → the whole body is one segment,
	// so manual authoring still works but auto-packing is deferred.
	got := segContents(SplitThread("a fairly long body with several words", 0))
	if len(got) != 1 {
		t.Errorf("unknown limit: want single segment, got %q", got)
	}
}

func TestSplitThread_Empty(t *testing.T) {
	if segs := SplitThread("   \n  \n", 280); len(segs) != 0 {
		t.Errorf("whitespace-only: want no segments, got %q", segContents(segs))
	}
	if segs := SplitThread("", 280); len(segs) != 0 {
		t.Errorf("empty: want no segments, got %q", segContents(segs))
	}
}

func TestSplitThread_AutoNeverExceedsLimit(t *testing.T) {
	limit := 40
	body := strings.Repeat("Lorem ipsum dolor sit amet. ", 30) +
		"\n\n" + strings.Repeat("x", 55) + " tail words here to spread things out"
	for _, s := range SplitThread(body, limit) {
		if n := utf8.RuneCountInString(s.Content); n > limit {
			t.Errorf("segment exceeds limit %d: %d runes: %q", limit, n, s.Content)
		}
	}
}
