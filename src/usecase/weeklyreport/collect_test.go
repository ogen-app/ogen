package weeklyreport

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
)

type fakeStore struct{ camps []repository.ReportCampaign }

func (fakeStore) Counts(context.Context, string, repository.ReportWindow) (repository.WeeklyCounts, error) {
	return repository.WeeklyCounts{MembersTotal: 3}, nil
}
func (fakeStore) PublishedPosts(context.Context, string, time.Time, time.Time) ([]repository.ReportPost, error) {
	return nil, nil
}
func (fakeStore) Failures(context.Context, string, time.Time, time.Time, int) ([]repository.ReportFailure, error) {
	return nil, nil
}
func (fakeStore) AttentionAccounts(context.Context, string, time.Time) ([]repository.ReportAccount, error) {
	return nil, nil
}
func (f fakeStore) GoalCampaigns(context.Context, string) ([]repository.ReportCampaign, error) {
	return f.camps, nil
}

type fakeAnalytics struct {
	cur, prev []models.PostAnalytics
	start     time.Time
}

func (f fakeAnalytics) PublishedBetween(_ context.Context, from, _ time.Time) ([]models.PostAnalytics, error) {
	if from.Equal(f.start) {
		return f.cur, nil
	}
	return f.prev, nil
}
func (fakeAnalytics) ReachByAgeSamples(context.Context) ([]repository.ReachAgeSample, error) {
	return nil, nil
}

type fakeFollowers struct {
	pts []repository.FollowerSeriesPoint
}

func (fakeFollowers) Summary(context.Context, string) ([]repository.FollowerAccountSummary, error) {
	return []repository.FollowerAccountSummary{{SocialAccountID: "a", CurrentFollowers: 130}}, nil
}
func (f fakeFollowers) Series(context.Context, repository.FollowerSeriesOptions) ([]repository.FollowerSeriesPoint, error) {
	return f.pts, nil
}

type fakeGoals struct{ gp *overview.GoalProgress }

func (f fakeGoals) Overview(context.Context, string) (*overview.Overview, error) {
	return &overview.Overview{Goal: f.gp}, nil
}

type failingLimits struct{}

func (failingLimits) Usage(context.Context, string) ([]entitlements.FeatureUsage, error) {
	return nil, errors.New("resolver down")
}

func TestCollect(t *testing.T) {
	w := testWindow(t)
	at := w.Start.Add(time.Hour)
	day := func(i int) time.Time { return time.Date(2026, 10, 11+i, 0, 0, 0, 0, time.UTC) }
	bucket := func(start time.Time, achieved int) overview.GoalBucket {
		return overview.GoalBucket{Start: start, End: start.AddDate(0, 0, 7), Target: 3, Achieved: achieved, Reached: achieved >= 3}
	}
	c := &Collector{
		Store: fakeStore{camps: []repository.ReportCampaign{{ID: "c1", Name: "Autumn"}}},
		Analytics: fakeAnalytics{start: w.Start,
			cur:  []models.PostAnalytics{{PostID: "p1", Reach: 900, Likes: 30, Comments: 10, Shares: 5, Platform: "LinkedIn", PublishedAt: &at}},
			prev: []models.PostAnalytics{{PostID: "p0", Reach: 600, Likes: 20}},
		},
		// Day 0 is the Sunday before the week: the follower level to compare against.
		Followers: fakeFollowers{pts: []repository.FollowerSeriesPoint{
			{PointDate: day(0), Followers: 100}, {PointDate: day(1), Followers: 110}, {PointDate: day(2), Followers: 108}, {PointDate: day(3), Followers: 130},
		}},
		Goals: fakeGoals{gp: &overview.GoalProgress{Cadence: "week", Buckets: []overview.GoalBucket{
			bucket(w.Start.AddDate(0, 0, -14), 3), bucket(w.Start.AddDate(0, 0, -7), 3), bucket(w.Start, 3), bucket(w.End, 0),
		}}},
		Limits:   failingLimits{},
		LimitPct: 80,
	}
	f, err := c.Collect(context.Background(), "t1", "Acme", w)
	if err != nil {
		t.Fatal(err)
	}
	a := f.Analytics
	if a == nil || a.Reach != 900 || a.Interactions != 45 || a.PrevReach != 600 || a.PrevInteractions != 20 || a.ReachByPost["p1"] != 900 {
		t.Fatalf("analytics = %+v", a)
	}
	if !a.HasFollowers || a.Followers != 130 || a.FollowersStart != 100 {
		t.Fatalf("followers = %d from %d", a.Followers, a.FollowersStart)
	}
	if want := []int{10, 0, 22, 0, 0, 0, 0}; !slices.Equal(a.FollowerGains, want) {
		t.Fatalf("gains = %v, want %v", a.FollowerGains, want)
	}
	if len(a.Top) != 1 || a.Top[0].PostID != "p1" {
		t.Fatalf("top = %+v", a.Top)
	}
	if len(f.Campaigns) != 1 || f.Campaigns[0].Streak != 3 || !f.Campaigns[0].Reached || f.Campaigns[0].Name != "Autumn" {
		t.Fatalf("campaigns = %+v", f.Campaigns)
	}
	if len(f.SectionErrors) != 1 || f.SectionErrors[0] != "limits" {
		t.Fatalf("section errors = %v", f.SectionErrors)
	}
	if f.ActiveMembers != -1 {
		t.Fatalf("active members without an activity reader = %d", f.ActiveMembers)
	}
}
