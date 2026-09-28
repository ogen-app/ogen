package queues

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
)

func TestShouldStopPaging(t *testing.T) {
	cases := []struct {
		name       string
		page       int
		items      int
		pagination zernio.AnalyticsPagination
		want       bool
	}{
		{name: "empty page", page: 1, items: 0, pagination: zernio.AnalyticsPagination{HasMore: true}, want: true},
		{name: "before explicit last page", page: 1, items: 5, pagination: zernio.AnalyticsPagination{TotalPages: 2}, want: false},
		{name: "explicit last page wins over short page", page: 2, items: 5, pagination: zernio.AnalyticsPagination{Pages: 2}, want: true},
		{name: "short page without hasMore", page: 1, items: 5, want: true},
		{name: "short page with hasMore", page: 1, items: 5, pagination: zernio.AnalyticsPagination{HasMore: true}, want: false},
		{name: "full page", page: 1, items: 10, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldStopPaging(tc.page, tc.items, 10, tc.pagination); got != tc.want {
				t.Fatalf("shouldStopPaging = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStampTimes(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-48 * time.Hour)
	changedAt := now.Add(-24 * time.Hour)
	prev := &models.PostAnalytics{FirstSeenAt: seen, LastChangedAt: changedAt}

	first := &models.PostAnalytics{}
	stampTimes(first, nil, now, true)
	if !first.FirstSeenAt.Equal(now) || !first.LastChangedAt.Equal(now) || !first.LastCheckedAt.Equal(now) {
		t.Fatalf("first sighting = %+v", first)
	}
	moved := &models.PostAnalytics{}
	stampTimes(moved, prev, now, true)
	if !moved.FirstSeenAt.Equal(seen) || !moved.LastChangedAt.Equal(now) {
		t.Fatalf("changed = %+v", moved)
	}
	same := &models.PostAnalytics{}
	stampTimes(same, prev, now, false)
	if !same.FirstSeenAt.Equal(seen) || !same.LastChangedAt.Equal(changedAt) || !same.LastCheckedAt.Equal(now) {
		t.Fatalf("unchanged = %+v", same)
	}
}
