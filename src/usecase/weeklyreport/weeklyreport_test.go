package weeklyreport

import (
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func TestDue(t *testing.T) {
	kyiv := mustLoc(t, "Europe/Kyiv")
	ny := mustLoc(t, "America/New_York")

	cases := []struct {
		name    string
		now     time.Time
		loc     *time.Location
		due     bool
		start   string
		endDate string
	}{
		// Monday 2026-10-19 09:00 in Kyiv is 06:00 UTC.
		{"kyiv before nine", time.Date(2026, 10, 19, 5, 59, 0, 0, time.UTC), kyiv, false, "", ""},
		{"kyiv at nine", time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC), kyiv, true, "2026-10-12", "2026-10-19"},
		{"kyiv late monday", time.Date(2026, 10, 19, 20, 59, 0, 0, time.UTC), kyiv, true, "2026-10-12", "2026-10-19"},
		{"kyiv tuesday", time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC), kyiv, false, "", ""},
		// The same UTC instant is still Monday 02:00 in New York: not yet due.
		{"new york early", time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC), ny, false, "", ""},
		{"new york at nine", time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC), ny, true, "2026-10-12", "2026-10-19"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, due := Due(tc.now, tc.loc, tc.loc.String())
			if due != tc.due {
				t.Fatalf("due = %v, want %v", due, tc.due)
			}
			if !due {
				return
			}
			if got := w.Start.Format(time.DateOnly); got != tc.start {
				t.Errorf("start = %s, want %s", got, tc.start)
			}
			if got := w.End.Format(time.DateOnly); got != tc.endDate {
				t.Errorf("end = %s, want %s", got, tc.endDate)
			}
			if w.Start.Hour() != 0 || w.Start.Location() != tc.loc {
				t.Errorf("start is not a local midnight: %v", w.Start)
			}
			if !w.PrevStart.Equal(w.Start.AddDate(0, 0, -7)) {
				t.Errorf("prev start = %v", w.PrevStart)
			}
		})
	}
}

// The week Europe/Kyiv leaves summer time is 169 hours long; days still split
// on local midnights.
func TestWindowAcrossDST(t *testing.T) {
	kyiv := mustLoc(t, "Europe/Kyiv")
	w, due := Due(time.Date(2026, 10, 26, 7, 0, 0, 0, time.UTC), kyiv, "Europe/Kyiv")
	if !due {
		t.Fatal("expected due")
	}
	if got := w.End.Sub(w.Start); got != 169*time.Hour {
		t.Fatalf("week length = %v, want 169h", got)
	}
	// Sunday 23:30 local, after the clocks went back, is still the last day.
	sunday := time.Date(2026, 10, 25, 23, 30, 0, 0, kyiv)
	if i := w.dayIndex(sunday); i != 6 {
		t.Fatalf("dayIndex(sunday) = %d, want 6", i)
	}
	if i := w.dayIndex(w.End); i != -1 {
		t.Fatalf("dayIndex(end) = %d, want -1", i)
	}
}

func testWindow(t *testing.T) Window {
	t.Helper()
	w, _ := Due(time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC).AddDate(0, 0, -1), mustLoc(t, "Europe/Kyiv"), "Europe/Kyiv")
	return w
}

func posts(w Window, n int) []repository.ReportPost {
	out := make([]repository.ReportPost, n)
	for i := range out {
		out[i] = repository.ReportPost{
			ID: "p" + string(rune('a'+i)), Platform: "LinkedIn", SocialAccountID: "acc1",
			AccountName: "Acme Co.", PublishedAt: w.Start.Add(time.Duration(i) * 25 * time.Hour),
		}
	}
	return out
}

func TestHeadline(t *testing.T) {
	w := testWindow(t)
	cases := []struct {
		name string
		f    Facts
		want string
	}{
		{"nothing went out", Facts{Window: w, Counts: repository.WeeklyCounts{Failed: 2}}, "Nothing went out last week."},
		{"reach up", Facts{Window: w, Published: posts(w, 12), Analytics: &AnalyticsFacts{Reach: 134, PrevReach: 100}}, "12 posts went out and reach is up 34%."},
		{"reach down", Facts{Window: w, Published: posts(w, 3), Analytics: &AnalyticsFacts{Reach: 60, PrevReach: 100}}, "3 posts went out; reach is down 40%."},
		{"quiet", Facts{Window: w, Published: posts(w, 2), Counts: repository.WeeklyCounts{PrevPublished: 6}}, "A quiet week: 2 posts went out."},
		{"one post", Facts{Window: w, Published: posts(w, 1)}, "1 post went out last week."},
		{"small reach change", Facts{Window: w, Published: posts(w, 5), Analytics: &AnalyticsFacts{Reach: 105, PrevReach: 100}}, "5 posts went out last week."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headline(tc.f); got != tc.want {
				t.Fatalf("headline = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLede(t *testing.T) {
	w := testWindow(t)
	f := Facts{
		Window: w, Published: posts(w, 2), Counts: repository.WeeklyCounts{Failed: 2},
		Analytics: &AnalyticsFacts{Top: []PerfPost{{Reach: 9600, Multiple: 1.94, Platform: "linkedin"}}},
	}
	want := "Your best post reached 9,600 people, 1.9× what a LinkedIn post of yours usually gets at that age. 2 posts didn't go out."
	if got := lede(f); got != want {
		t.Fatalf("lede = %q\nwant %q", got, want)
	}
	f = Facts{Window: w, Published: posts(w, 2), Counts: repository.WeeklyCounts{ScheduledNext: 9}}
	if got := lede(f); got != "Nothing failed, and 9 posts are lined up for next week." {
		t.Fatalf("quiet lede = %q", got)
	}
}

func TestRenderSections(t *testing.T) {
	w := testWindow(t)
	exp := w.Now.Add(72 * time.Hour)
	f := Facts{
		Window:    w,
		Workspace: "Acme Studio",
		Published: posts(w, 3),
		Counts: repository.WeeklyCounts{
			PrevPublished: 2, Failed: 1, PrevFailed: 2, ScheduledNext: 9, ManualDue: 2,
			IdeasWaiting: 7, IdeasCreated: 18, MembersTotal: 6, MembersJoined: 1, InvitesPending: 1,
		},
		Failures: []repository.ReportFailure{{
			PostID: "f1", CampaignID: "c1", Content: "Autumn lookbook carousel", Platform: "Instagram",
			SocialAccountID: "acc2", AccountName: "@acme", Reason: "Image 3 has an unsupported aspect ratio.", At: w.Start.Add(80 * time.Hour),
		}},
		Attention: []repository.ReportAccount{
			{ID: "acc1", Platform: "linkedin", Name: "Acme Co.", NeedsReconnect: true, Scheduled: 3},
			{ID: "acc3", Platform: "youtube", Name: "Acme", TokenExpiresAt: &exp},
		},
		Limits:        []entitlements.FeatureUsage{{Key: "content_bank_assets", Name: "Content bank assets", Current: 46, Limit: 50}},
		Campaigns:     []CampaignGoal{{ID: "c1", Name: "Autumn launch", Cadence: "week", Target: 5, Achieved: 4, Streak: 0}, {ID: "c2", Name: "Evergreen", Cadence: "week", Target: 3, Achieved: 3, Reached: true, Streak: 3}},
		ActiveMembers: 4,
		Analytics: &AnalyticsFacts{
			ReachByPost: map[string]int{"pa": 9600, "pb": 300}, Reach: 9900, PrevReach: 9000,
			Interactions: 440, PrevInteractions: 300, HasFollowers: true, Followers: 8912, FollowersStart: 8766,
			FollowerGains: []int{18, 22, 31, 25, 19, 14, 17},
		},
	}
	v := Render(f, "https://app.example/")

	if v.RangeLabel != "Oct 12 – 18, 2026" {
		t.Errorf("range label = %q", v.RangeLabel)
	}
	if !strings.HasPrefix(v.AppURL, "https://app.example/?utm_source=weekly_report") {
		t.Errorf("app url = %q", v.AppURL)
	}
	if len(v.Attention) != 3 || v.Attention[0].Text != "LinkedIn · Acme Co. needs reconnecting" ||
		v.Attention[0].Detail != "3 scheduled posts are waiting on it." ||
		!strings.Contains(v.Attention[1].Text, "disconnects on") ||
		v.Attention[2].Text != "Content bank assets: 46 of 50 used" {
		t.Errorf("attention = %+v", v.Attention)
	}

	p := v.Publishing
	if p == nil || p.Published.Value != "3" || p.Published.Delta != "↑ 50% vs last week" || !p.Published.Good {
		t.Fatalf("publishing = %+v", p)
	}
	if p.Failed != "1" || p.FailedNote != "1 fewer" {
		t.Errorf("failed = %q note %q", p.Failed, p.FailedNote)
	}
	if p.Bars[0].Height != publishingChartPx || p.Bars[0].Value != "1" || p.Bars[6].Height != 0 {
		t.Errorf("bars = %+v", p.Bars)
	}
	if len(p.Channels) != 2 || p.Channels[0].Published != "3" || p.Channels[0].Reach != "9,900" ||
		p.Channels[1].Failed != "1" || !p.Channels[1].HasFailed || p.Channels[1].Reach != "–" {
		t.Errorf("channels = %+v", p.Channels)
	}
	if len(p.Failures) != 1 || p.Failures[0].Title != "Autumn lookbook carousel" ||
		!strings.Contains(p.Failures[0].URL, "/campaigns/c1/posts/f1") {
		t.Errorf("failures = %+v", p.Failures)
	}
	if p.NextWeek != "9 posts scheduled · 2 to publish by hand" {
		t.Errorf("next week = %q", p.NextWeek)
	}

	perf := v.Performance
	if perf == nil || perf.Reach.Value != "9,900" || perf.Reach.Delta != "↑ 10%" ||
		perf.Engagement.Value != "4.4%" || perf.Engagement.Delta != "↑ 1.1 pt" ||
		perf.Followers.Value != "8,912" || perf.Followers.Delta != "+146" || len(perf.FollowerBars) != 7 {
		t.Errorf("performance = %+v", perf)
	}

	c := v.Campaigns
	if c == nil || len(c.Rows) != 2 || c.Rows[0].Status != "4 of 5 this week" || c.Rows[0].Fill != 112 || c.Rows[0].Rest != 28 ||
		c.Rows[1].Status != "Goal met" || c.Rows[1].Goal != "Goal: 3 posts a week · 3-week streak" {
		t.Errorf("campaigns = %+v", c)
	}
	if v.Ideas == nil || v.Ideas.Waiting != "7 ideas are waiting for your call" || v.Ideas.Summary != "18 new ideas" {
		t.Errorf("ideas = %+v", v.Ideas)
	}
	if v.Team == nil || v.Team.Summary != "4 of 6 members were active · 1 member joined · 1 invite pending" {
		t.Errorf("team = %+v", v.Team)
	}
}

func TestRenderDropsEmptySections(t *testing.T) {
	w := testWindow(t)
	v := Render(Facts{Window: w, Published: posts(w, 2), Counts: repository.WeeklyCounts{MembersTotal: 1}, ActiveMembers: 1}, "https://app.example")
	if v.Publishing == nil {
		t.Fatal("publishing should render")
	}
	if v.Publishing.Failed != "" || len(v.Publishing.Failures) != 0 || v.Publishing.NextWeek != "" {
		t.Errorf("publishing has empty parts: %+v", v.Publishing)
	}
	if v.Performance != nil || v.Campaigns != nil || v.Ideas != nil || v.Team != nil || len(v.Attention) != 0 {
		t.Errorf("empty sections rendered: %+v", v)
	}
}

func TestEmpty(t *testing.T) {
	w := testWindow(t)
	exp := w.Now
	if !(Facts{Window: w, Attention: []repository.ReportAccount{{NeedsReconnect: true, TokenExpiresAt: &exp}}}).Empty() {
		t.Error("attention alone should be empty")
	}
	if (Facts{Window: w, Counts: repository.WeeklyCounts{IdeasCreated: 1}}).Empty() {
		t.Error("a new idea is activity")
	}
	if (Facts{Window: w, Published: posts(w, 1)}).Empty() {
		t.Error("a published post is activity")
	}
}

func TestCount(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 2140: "2,140", 9999: "9,999", 12_900: "12.9K", 48_200: "48.2K", 10_000: "10K", 1_250_000: "1.3M"} {
		if got := count(n); got != want {
			t.Errorf("count(%d) = %q, want %q", n, got, want)
		}
	}
}
