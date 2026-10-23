package templates

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func sampleWeeklyReport() *models.WeeklyReportView {
	bars := []models.WeeklyBar{{Day: "M", Value: "2", Height: 36}, {Day: "T", Value: "4", Height: 72}, {Day: "W"}, {Day: "T"}, {Day: "F"}, {Day: "S"}, {Day: "S"}}
	return &models.WeeklyReportView{
		SchemaVersion: models.WeeklyReportSchemaVersion,
		Workspace:     "Acme <Studio>",
		RangeLabel:    "Oct 12 – 18, 2026",
		WeekOf:        "Oct 12",
		Timezone:      "Europe/Kyiv",
		Headline:      "12 posts went out and reach is up 34%.",
		Lede:          "Your best post reached 9,600 people.",
		AppURL:        "https://app.example/?utm_source=weekly_report",
		Attention:     []models.WeeklyAttention{{Text: "LinkedIn · Acme Co. needs reconnecting", Detail: "3 scheduled posts are waiting on it.", LinkLabel: "Reconnect", URL: "https://app.example/workspace-settings"}},
		Publishing: &models.WeeklyPublishing{
			Published: models.WeeklyFigure{Value: "12", Delta: "↑ 33% vs last week", Good: true},
			Failed:    "2", FailedNote: "1 fewer", ChartHeight: 72, Bars: bars,
			Channels:    []models.WeeklyChannel{{Platform: "LinkedIn", Account: "Acme Co.", Published: "5", Failed: "1", HasFailed: true, Reach: "31.4K"}},
			Failures:    []models.WeeklyFailure{{Title: "Autumn lookbook", Meta: "Instagram · Thu, Oct 16", Reason: "Unsupported aspect ratio.", URL: "https://app.example/calendar"}},
			FailuresURL: "https://app.example/calendar", NextWeek: "9 posts scheduled", CalendarURL: "https://app.example/calendar",
		},
		Performance: &models.WeeklyPerformance{
			Reach: models.WeeklyFigure{Value: "48.2K", Delta: "↑ 34%", Good: true}, Interactions: models.WeeklyFigure{Value: "2,140"},
			Engagement: models.WeeklyFigure{Value: "4.4%", Delta: "↓ 0.3 pt"}, Followers: models.WeeklyFigure{Value: "8,912", Delta: "+146", Good: true},
			FollowerHeight: 62, FollowerBars: bars,
			Top:   []models.WeeklyPost{{Rank: "1", Title: "We cut onboarding", Meta: "LinkedIn · Tue, Oct 14", Reach: "9,600 reach", Multiple: "1.9× typical", URL: "https://app.example/calendar"}},
			Below: []models.WeeklyPost{{Title: "Webinar reminder", Meta: "X · Fri, Oct 17", Reach: "310 reach", Multiple: "0.4× typical", URL: "https://app.example/calendar"}},
			URL:   "https://app.example/analytics",
		},
		Campaigns: &models.WeeklyCampaigns{Rows: []models.WeeklyCampaign{{Name: "Autumn launch", Goal: "Goal: 5 posts a week", Status: "4 of 5 this week", Fill: 112, Rest: 28, URL: "https://app.example/campaigns/c1"}}, Summary: "1 new campaign", URL: "https://app.example/campaigns"},
		Ideas:     &models.WeeklyIdeas{Waiting: "7 ideas are waiting for your call", Summary: "18 new ideas", URL: "https://app.example/ideas"},
		Team:      &models.WeeklyTeam{Summary: "4 of 6 members were active"},
	}
}

// TestRenderWeeklyReport proves the weekly_report template parses and renders
// every section from the stored view, escaping workspace-supplied text.
func TestRenderWeeklyReport(t *testing.T) {
	tmpl := defaultByKey(t, KeyWeeklyReport)
	r, err := Render(tmpl, Data{Name: "Ann", Report: sampleWeeklyReport(), UnsubscribeURL: "https://api.example/api/email/weekly-report/unsubscribe?token=abc"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.Subject != "Acme <Studio> · week of Oct 12: 12 posts went out and reach is up 34%." {
		t.Errorf("subject = %q", r.Subject)
	}
	wants := []string{
		"12 posts went out", "Reconnect", "31.4K", "Autumn lookbook", "9 posts scheduled",
		"48.2K", "↓ 0.3 pt", "1.9× typical", "Below typical", "Autumn launch",
		"7 ideas are waiting", "4 of 6 members were active", "weekly-report/unsubscribe?token=abc",
	}
	for _, body := range []string{r.HTML, r.Text} {
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q", want)
			}
		}
	}
	for _, want := range []string{"Needs your attention", "New followers per day"} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("html missing %q", want)
		}
	}
	if !strings.Contains(r.HTML, "Acme &lt;Studio&gt;") || strings.Contains(r.HTML, "Acme <Studio>") {
		t.Error("workspace name is not HTML-escaped")
	}
	if !strings.Contains(r.HTML, `height="72"`) || !strings.Contains(r.HTML, "height:72px") {
		t.Error("bar heights not rendered into the table cells")
	}
}

// A report with only a headline renders no section markup.
func TestRenderWeeklyReportMinimal(t *testing.T) {
	tmpl := defaultByKey(t, KeyWeeklyReport)
	r, err := Render(tmpl, Data{Report: &models.WeeklyReportView{Workspace: "Acme", WeekOf: "Oct 12", Headline: "Nothing went out last week.", Timezone: "UTC"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, absent := range []string{"Needs your attention", "Publishing", "Performance", "Campaigns", "Ideas and content bank", "Team"} {
		if strings.Contains(r.HTML, absent) {
			t.Errorf("minimal report rendered %q", absent)
		}
	}
}
