package pgtest

import (
	"fmt"
	"os"
	"testing"
)

func TestCreatorAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		// This process: its own databases are never swept.
		{fmt.Sprintf("ogen_test_%d_1", os.Getpid()), true},
		// The parent (the go tool or ginkgo) is alive too: a sibling process.
		{fmt.Sprintf("ogen_test_%d_3", os.Getppid()), true},
		// No such process: an orphan from an earlier run.
		{"ogen_test_2147483646_1", false},
		// Unparseable names fall back to DROP DATABASE's in-use check.
		{"ogen_test_abc_1", false},
		{"ogen_test_", false},
		{"other_db", false},
	} {
		if got := creatorAlive(tc.name); got != tc.want {
			t.Errorf("creatorAlive(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
