package queues

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestFetchFromFollowsTheOldestDuePost(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	p := &RefreshZernioAnalyticsProcessor{WindowDays: 90}
	ago := func(d time.Duration) *time.Time { return new(now.Add(-d)) }
	checked := func(d time.Duration) *models.PostAnalytics {
		return &models.PostAnalytics{LastCheckedAt: now.Add(-d)}
	}

	posts := []models.Post{
		{ID: "fresh", PublishedAt: ago(24 * time.Hour)},        // fresh bucket, hourly
		{ID: "settled", PublishedAt: ago(60 * 24 * time.Hour)}, // cold bucket, weekly
	}

	// The settled post was checked yesterday, so only the fresh one is due and
	// the fetch starts the day before it was published.
	current := map[string]*models.PostAnalytics{"fresh": checked(2 * time.Hour), "settled": checked(24 * time.Hour)}
	from, due := p.fetchFrom(posts, current, now)
	if !due || from != "2026-02-27" {
		t.Fatalf("fetchFrom = (%s, %v), want (2026-02-27, true)", from, due)
	}

	// Once the settled post is due too, the fetch reaches back to it.
	current["settled"] = checked(8 * 24 * time.Hour)
	if from, _ := p.fetchFrom(posts, current, now); from != "2025-12-30" {
		t.Fatalf("fetchFrom with the settled post due = %s, want 2025-12-30", from)
	}

	// Nothing due: no fetch at all.
	current = map[string]*models.PostAnalytics{"fresh": checked(time.Minute), "settled": checked(time.Hour)}
	if _, due := p.fetchFrom(posts, current, now); due {
		t.Fatal("fetchFrom reported a due post when every post was just checked")
	}

	// A due post with no publish time keeps the full window.
	posts = append(posts, models.Post{ID: "undated"})
	if from, _ := p.fetchFrom(posts, current, now); from != "2025-12-01" {
		t.Fatalf("fetchFrom with an undated due post = %s, want the window start 2025-12-01", from)
	}
}
