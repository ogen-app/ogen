package brand_distill

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// mdLink is a markdown link or image; its text is what a reader sees.
var mdLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)

// typography folds the characters a quote may differ in without changing
// what it says: curly quotes, dashes, non-breaking spaces and markdown
// emphasis.
var typography = strings.NewReplacer(
	"‘", "'", "’", "'", "‚", "'", "′", "'",
	"“", `"`, "”", `"`, "„", `"`, "″", `"`,
	"–", "-", "—", "-", "−", "-",
	" ", " ", " ", " ", " ", " ",
	"…", "...",
	// untag writes < and > as ‹ and › into the prompt; a quote copied from
	// there still matches its source.
	"‹", "<", "›", ">",
	"**", "", "__", "", "*", "", "`", "",
)

// Normalize is the form two texts are compared in: NFC, markdown links read
// as their text, typography folded, whitespace collapsed, lower case.
func Normalize(s string) string {
	s = norm.NFC.String(s)
	s = mdLink.ReplaceAllString(s, "$1")
	s = typography.Replace(s)
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Verbatim reports whether quote appears in text word for word, as
// Normalize compares them. An empty quote never does.
func Verbatim(text, quote string) bool {
	q := Normalize(quote)
	return q != "" && strings.Contains(Normalize(text), q)
}
