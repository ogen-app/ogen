package textdiff

import (
	"slices"
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
		want          []Change
	}{
		{"same", "a\nb", "a\nb", nil},
		{"one line rewritten", "Hook\nOld claim.\nCTA", "Hook\nNew claim.\nCTA", []Change{{Before: "Old claim.", After: "New claim."}}},
		{"a run rewritten as one", "a\nb\nc\nd", "a\nB\nC\nd", []Change{{Before: "b\nc", After: "B\nC"}}},
		{"added", "a\nc", "a\nb\nc", []Change{{After: "b"}}},
		{"removed", "a\nb\nc", "a\nc", []Change{{Before: "b"}}},
		{"two apart", "a\nb\nc\nd\ne", "a\nB\nc\nd\nE", []Change{{Before: "b", After: "B"}, {Before: "e", After: "E"}}},
		{"from nothing", "", "a", []Change{{After: "a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Lines(tt.before, tt.after); !slices.Equal(got, tt.want) {
				t.Fatalf("Lines = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A text too long to compare by lines is one change of the whole.
func TestLines_TooLong(t *testing.T) {
	long := strings.Repeat("line\n", MaxLines+1)
	if got := Lines(long, long+"more"); len(got) != 1 || got[0].Before != long {
		t.Fatalf("Lines = %d changes", len(got))
	}
}
