package textdiff

import (
	"slices"
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	none := []string{}
	tests := []struct {
		name          string
		before, after string
		want          []Change
	}{
		{"same", "a\nb", "a\nb", nil},
		{"one line rewritten", "Hook\nOld claim.\nCTA", "Hook\nNew claim.\nCTA", []Change{{Before: []string{"Old claim."}, After: []string{"New claim."}}}},
		{"a run rewritten as one", "a\nb\nc\nd", "a\nB\nC\nd", []Change{{Before: []string{"b", "c"}, After: []string{"B", "C"}}}},
		{"added", "a\nc", "a\nb\nc", []Change{{Before: none, After: []string{"b"}}}},
		{"removed", "a\nb\nc", "a\nc", []Change{{Before: []string{"b"}, After: none}}},
		{"two apart", "a\nb\nc\nd\ne", "a\nB\nc\nd\nE", []Change{{Before: []string{"b"}, After: []string{"B"}}, {Before: []string{"e"}, After: []string{"E"}}}},
		{"from nothing", "", "a", []Change{{Before: none, After: []string{"a"}}}},
		// A blank line added and one removed read apart.
		{"blank line added", "a", "a\n", []Change{{Before: none, After: []string{""}}}},
		{"blank line removed", "a\n", "a", []Change{{Before: []string{""}, After: none}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Lines(tt.before, tt.after)
			if !slices.EqualFunc(got, tt.want, func(x, y Change) bool {
				return slices.Equal(x.Before, y.Before) && slices.Equal(x.After, y.After) && (x.Before == nil) == (y.Before == nil) && (x.After == nil) == (y.After == nil)
			}) {
				t.Fatalf("Lines = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// A text too long to compare by lines is one change of the whole.
func TestLines_TooLong(t *testing.T) {
	long := strings.Repeat("line\n", MaxLines+1)
	if got := Lines(long, long+"more"); len(got) != 1 || len(got[0].Before) != MaxLines+2 {
		t.Fatalf("Lines = %d changes", len(got))
	}
}
