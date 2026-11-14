// Package textdiff says how a text's lines changed: each run of lines
// rewritten, removed or added, so a client can strike the old above the new.
package textdiff

import (
	"slices"
	"strings"
)

// MaxLines is the most lines a side is compared by; a longer text reads as
// one change of the whole.
const MaxLines = 400

// Change is a run of lines rewritten: Before the lines it had, After the
// lines it has now, each joined by "\n". Before is empty for lines added,
// After for lines removed.
type Change struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// Lines is how after's lines differ from before's, in order: the lines
// they share are left out. nil when the texts are the same.
func Lines(before, after string) []Change {
	if before == after {
		return nil
	}
	a, b := split(before), split(after)
	if len(a) > MaxLines || len(b) > MaxLines {
		return []Change{{Before: before, After: after}}
	}
	// keep[i][j] is the length of the longest run a[i:] and b[j:] share.
	keep := make([][]int, len(a)+1)
	for i := range keep {
		keep[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				keep[i][j] = keep[i+1][j+1] + 1
			} else {
				keep[i][j] = max(keep[i+1][j], keep[i][j+1])
			}
		}
	}
	var out []Change
	var was, now []string
	flush := func() {
		if len(was) > 0 || len(now) > 0 {
			out = append(out, Change{Before: strings.Join(was, "\n"), After: strings.Join(now, "\n")})
			was, now = nil, nil
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			flush()
			i, j = i+1, j+1
		case j < len(b) && (i == len(a) || keep[i][j+1] >= keep[i+1][j]):
			now = append(now, b[j])
			j++
		default:
			was = append(was, a[i])
			i++
		}
	}
	flush()
	return slices.Clip(out)
}

// split is s's lines; none for an empty text.
func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
