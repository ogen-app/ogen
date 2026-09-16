package platforms

import "testing"

// TestFlattenSocialText mirrors the front end's socialText.test.ts case-for-case
// (CON-126/CON-284): the Go server is the source of truth, so the two flatteners
// must agree on the plain text — and therefore the length — a caption publishes as.
func TestFlattenSocialText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strips emphasis but keeps words", "**bold** and *italic* and ~~gone~~", "bold and italic and gone"},
		{"keeps non-emphasis asterisks", "2 * 3 * 4", "2 * 3 * 4"},
		{"drops heading and quote markers", "## Why now\n\n> because", "Why now\n\nbecause"},
		{"bullets become bullet chars", "- one\n- two", "• one\n• two"},
		{"keeps ordered numbers", "1. one\n2. two", "1. one\n2. two"},
		{"keeps URL alongside link text", "[Ogen](https://getogen.com)", "Ogen (https://getogen.com)"},
		{"no repeat when text == url", "[https://getogen.com](https://getogen.com)", "https://getogen.com"},
		{"image reduces to alt text", "![a chart](https://x.com/a.png)", "a chart"},
		{"collapses block spacing", "a\n\n\n\nb", "a\n\nb"},
		{"unescapes escaped punctuation", "50\\% off \\*not italic\\*", "50% off *not italic*"},
		{"fenced code passes through", "```\nconst a = **b**\n```", "const a = **b**"},
		{"drops horizontal rules", "a\n\n---\n\nb", "a\n\nb"},
		{"drops asterisk rules too", "a\n\n***\n\nb", "a\n\nb"},
		{"empty input", "", ""},
		{"keeps intra-word underscores", "snake_case_name", "snake_case_name"},
		{"inline code", "run `go build` now", "run go build now"},
		{"autolink", "see <https://getogen.com>", "see https://getogen.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FlattenSocialText(tc.in); got != tc.want {
				t.Errorf("FlattenSocialText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVisibleLen is the property the thread pipeline actually leans on: Markdown
// syntax must not spend the per-message character budget.
func TestVisibleLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"**bold**", 4},                     // not 8
		{"[Ogen](https://getogen.com)", 26}, // "Ogen (https://getogen.com)"
		{"## Title", 5},                     // "Title"
		{"plain", 5},
		{"", 0},
	}
	for _, tc := range cases {
		if got := VisibleLen(tc.in); got != tc.want {
			t.Errorf("VisibleLen(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
