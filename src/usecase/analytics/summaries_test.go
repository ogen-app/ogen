package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// The best slot leads, the runner-up is the best on another day, the
// weakest is the last ranked, and a slot with too few posts isn't ranked.
func TestSummarizeBestTimes(t *testing.T) {
	s := SummarizeBestTimes([]zernio.BestTimeSlot{
		{DayOfWeek: 4, Hour: 18, AvgEngagement: 500, PostCount: 9},
		{DayOfWeek: 4, Hour: 19, AvgEngagement: 450, PostCount: 5},
		{DayOfWeek: 2, Hour: 18, AvgEngagement: 400, PostCount: 4},
		{DayOfWeek: 1, Hour: 9, AvgEngagement: 900, PostCount: 1},
		{DayOfWeek: 0, Hour: 7, AvgEngagement: 50, PostCount: 3},
	})
	if s.Unit != UnitAvgInteractions || s.MinPosts != MinSlotPosts || s.TooFew {
		t.Fatalf("summary = %+v", s)
	}
	if s.Best == nil || s.Best.DayOfWeek != 4 || s.Best.Hour != 18 || s.Best.Posts != 9 {
		t.Fatalf("best = %+v", s.Best)
	}
	if s.RunnerUp == nil || s.RunnerUp.DayOfWeek != 2 || s.RunnerUp.Hour != 18 {
		t.Fatalf("runner-up = %+v", s.RunnerUp)
	}
	if s.Weakest == nil || s.Weakest.DayOfWeek != 0 || s.Weakest.AvgInteractions != 50 {
		t.Fatalf("weakest = %+v", s.Weakest)
	}

	if s := SummarizeBestTimes([]zernio.BestTimeSlot{{DayOfWeek: 1, Hour: 9, AvgEngagement: 900, PostCount: 2}}); !s.TooFew || s.Best != nil {
		t.Fatalf("too few = %+v", s)
	}
	// One day only: no runner-up, and the weakest is the other slot.
	s = SummarizeBestTimes([]zernio.BestTimeSlot{
		{DayOfWeek: 3, Hour: 9, AvgEngagement: 10, PostCount: 3},
		{DayOfWeek: 3, Hour: 10, AvgEngagement: 20, PostCount: 3},
	})
	if s.RunnerUp != nil || s.Weakest == nil || s.Weakest.Hour != 9 {
		t.Fatalf("one day = %+v", s)
	}
	// The runner-up isn't also named the weakest.
	s = SummarizeBestTimes([]zernio.BestTimeSlot{
		{DayOfWeek: 3, Hour: 9, AvgEngagement: 20, PostCount: 3},
		{DayOfWeek: 4, Hour: 9, AvgEngagement: 10, PostCount: 3},
	})
	if s.RunnerUp == nil || s.Weakest != nil {
		t.Fatalf("two days = %+v", s)
	}
}

// The half-life is read off the cumulative curve between the buckets
// either side of 50%.
func TestDecayHalfLife(t *testing.T) {
	h := DecayHalfLife([]zernio.ContentDecayBucket{
		{BucketOrder: 1, BucketLabel: "6-12h", AvgPctOfFinal: 70},
		{BucketOrder: 0, BucketLabel: "0-6h", AvgPctOfFinal: 30},
		{BucketOrder: 2, BucketLabel: "1-3d", AvgPctOfFinal: 95},
	})
	if h == nil || h.Bucket != "6-12h" || h.Hours != 9 {
		t.Fatalf("half-life = %+v", h)
	}
	if h := DecayHalfLife([]zernio.ContentDecayBucket{{BucketLabel: "0-6h", AvgPctOfFinal: 60}}); h == nil || h.Hours != 5 {
		t.Fatalf("first bucket = %+v", h)
	}
	if h := DecayHalfLife([]zernio.ContentDecayBucket{{BucketLabel: "0-2d", AvgPctOfFinal: 40}, {BucketOrder: 1, BucketLabel: "2d+", AvgPctOfFinal: 100}}); h == nil || h.Bucket != "2d+" || h.Hours != 0 {
		t.Fatalf("open bucket = %+v", h)
	}
	if h := DecayHalfLife([]zernio.ContentDecayBucket{{BucketLabel: "0-6h", AvgPctOfFinal: 40}}); h != nil {
		t.Fatalf("never half = %+v", h)
	}
	for label, want := range map[string]float64{"0-6h": 6, "12h": 12, "1-3d": 72, "1-2w": 336} {
		if got, ok := bucketEndHours(label); !ok || got != want {
			t.Errorf("%s = %v, %v", label, got, ok)
		}
	}
	if _, ok := bucketEndHours("week one"); ok {
		t.Error("an unreadable label has an age")
	}
}

func at(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

// Posts count into their ISO weeks; the average is over the full weeks,
// and last month is the calendar month before now's.
func TestCadenceOf(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) // a Thursday
	c := CadenceOf([]models.PostAnalytics{
		{Platform: "LinkedIn", PublishedAt: at("2026-11-10T09:00:00Z")}, // this week
		{Platform: "LinkedIn", PublishedAt: at("2026-11-03T09:00:00Z")},
		{Platform: "Instagram", PublishedAt: at("2026-11-01T23:00:00Z")}, // the Sunday before
		{Platform: "Instagram", PublishedAt: at("2026-10-15T09:00:00Z")},
		{Platform: "Mastodon", PublishedAt: at("2026-08-01T09:00:00Z")}, // before the 12 weeks
		{Platform: "LinkedIn"},
	}, map[string]string{"linkedin": "linkedin", "instagram": "instagram"}, now)

	if len(c.Weeks) != CadenceWeeks || c.Weeks[CadenceWeeks-1].WeekStart != "2026-11-09" || !c.Weeks[CadenceWeeks-1].Current || c.Weeks[CadenceWeeks-1].Posts != 1 {
		t.Fatalf("this week = %+v", c.Weeks[len(c.Weeks)-1])
	}
	if c.Weeks[0].WeekStart != "2026-08-24" || c.Weeks[CadenceWeeks-2].Posts != 1 || c.Weeks[CadenceWeeks-3].Posts != 1 {
		t.Fatalf("weeks = %+v", c.Weeks)
	}
	if c.PerWeek != 0.3 || c.LastMonth != 1 {
		t.Fatalf("per week %v, last month %d", c.PerWeek, c.LastMonth)
	}
	if len(c.Platforms) != 2 || c.Platforms[0] != (PlatformCadence{Platform: "instagram", PerWeek: 0.2}) || c.Platforms[1] != (PlatformCadence{Platform: "linkedin", PerWeek: 0.1}) {
		t.Fatalf("platforms = %+v", c.Platforms)
	}
}

type publishedRows struct {
	repository.PostAnalyticsRepository
	rows     []models.PostAnalytics
	from, to time.Time
}

func (p *publishedRows) PublishedBetween(_ context.Context, from, to time.Time) ([]models.PostAnalytics, error) {
	p.from, p.to = from, to
	return p.rows, nil
}

// The biggest day sums the accounts' day-over-day gains, skips gaps, and
// names the post that reached most that day or the day before.
func TestFollowerHistory_BiggestDay(t *testing.T) {
	day := func(s string) time.Time { t, _ := time.Parse(time.DateOnly, s); return t }
	points := []repository.FollowerSeriesPoint{
		{SocialAccountID: "a", PointDate: day("2026-04-14"), Followers: 100},
		{SocialAccountID: "a", PointDate: day("2026-04-15"), Followers: 140},
		{SocialAccountID: "a", PointDate: day("2026-04-20"), Followers: 400}, // after a gap
		{SocialAccountID: "b", PointDate: day("2026-04-14"), Followers: 10},
		{SocialAccountID: "b", PointDate: day("2026-04-15"), Followers: 27},
		{SocialAccountID: "b", PointDate: day("2026-04-16"), Followers: 60},
	}
	got := biggestDay(points)
	if got == nil || got.Date != "2026-04-15" || got.Gain != 57 {
		t.Fatalf("biggest = %+v", got)
	}
	if biggestDay(points[:1]) != nil {
		t.Fatal("one point has a biggest day")
	}

	repo := &publishedRows{rows: []models.PostAnalytics{{PostID: "p1", Title: "Menu", Reach: 10}, {PostID: "p2", Title: "Thermal pool reel", Reach: 90}}}
	s := &Service{Repo: repo}
	if err := s.postOfDay(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	if got.PostID != "p2" || got.PostTitle != "Thermal pool reel" || repo.from.Format(time.DateOnly) != "2026-04-14" || repo.to.Format(time.DateOnly) != "2026-04-16" {
		t.Fatalf("post = %+v, read %v to %v", got, repo.from, repo.to)
	}
}
