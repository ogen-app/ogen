package zernio

import (
	"maps"
	"testing"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// A post naming an account waits on it; one naming none waits on its
// platform's only account, and on no one when the platform has two. Every
// account gets an entry, zero when nothing waits on it.
func TestWaitingByAccount(t *testing.T) {
	li, ig := LookupSqidByZernioID("linkedin"), LookupSqidByZernioID("instagram")
	if li == "" || ig == "" {
		t.Fatal("the built-in catalog lacks linkedin or instagram")
	}
	counts := []repository.ScheduledCount{
		{PlatformID: li, Count: 5},
		{PlatformID: li, AccountID: "li1", Count: 2},
		{PlatformID: ig, Count: 3},
		{PlatformID: ig, AccountID: "ig1", Count: 1},
	}
	got := WaitingByAccount(counts, map[string]string{"li1": "linkedin", "li2": "linkedin", "ig1": "instagram", "x1": "twitter"})
	want := map[string]int{"li1": 2, "li2": 0, "ig1": 4, "x1": 0}
	if !maps.Equal(got, want) {
		t.Fatalf("waiting = %v, want %v", got, want)
	}
}
