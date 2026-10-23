package weeklyreport

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Chart and progress-bar sizes, in pixels, matching the email design.
const (
	publishingChartPx = 72
	followerChartPx   = 62
	goalBarPx         = 140
	maxChannels       = 6
)

var dayLetters = [7]string{"M", "T", "W", "T", "F", "S", "S"}

// links builds the app deep links. Every link carries the report's UTM tags.
type links struct{ base string }

func (l links) to(path string) string {
	return l.base + path + "?utm_source=weekly_report&utm_medium=email"
}

func (l links) post(postID, campaignID string) string {
	if campaignID == "" {
		return l.to("/calendar")
	}
	return l.to("/campaigns/" + campaignID + "/posts/" + postID)
}

// Render turns a week's facts into the email view. appBaseURL is the app's
// absolute origin.
func Render(f Facts, appBaseURL string) models.WeeklyReportView {
	l := links{base: strings.TrimRight(appBaseURL, "/")}
	w := f.Window
	v := models.WeeklyReportView{
		SchemaVersion: models.WeeklyReportSchemaVersion,
		Workspace:     f.Workspace,
		RangeLabel:    rangeLabel(w.Start, w.End),
		WeekOf:        w.Start.Format("Jan 2"),
		Timezone:      w.Zone,
		AppURL:        l.to("/"),
		Attention:     renderAttention(f, l),
		Publishing:    renderPublishing(f, l),
		Performance:   renderPerformance(f, l),
		Campaigns:     renderCampaigns(f, l),
		Ideas:         renderIdeas(f, l),
		Team:          renderTeam(f),
	}
	v.Headline = headline(f)
	v.Lede = lede(f)
	return v
}

func renderAttention(f Facts, l links) []models.WeeklyAttention {
	var out []models.WeeklyAttention
	for _, a := range f.Attention {
		item := models.WeeklyAttention{LinkLabel: "Reconnect", URL: l.to("/workspace-settings")}
		label := platformName(a.Platform) + " · " + a.Name
		switch {
		case a.NeedsReconnect:
			item.Text = label + " needs reconnecting"
		case a.TokenExpiresAt != nil && a.TokenExpiresAt.Before(f.Window.Now):
			item.Text = label + " needs reconnecting"
		case a.TokenExpiresAt != nil:
			item.Text = label + " disconnects on " + a.TokenExpiresAt.In(f.Window.Loc).Format("Mon, Jan 2")
		default:
			continue
		}
		if a.Scheduled > 0 {
			item.Detail = plural(a.Scheduled, "scheduled post is", "scheduled posts are") + " waiting on it."
		}
		out = append(out, item)
	}
	for _, u := range f.Limits {
		out = append(out, models.WeeklyAttention{
			Text:      fmt.Sprintf("%s: %s of %s used", u.Name, grouped(int(u.Current)), grouped(int(u.Limit))),
			LinkLabel: "View plan",
			URL:       l.to("/plans"),
		})
	}
	return out
}

func renderPublishing(f Facts, l links) *models.WeeklyPublishing {
	c := f.Counts
	if len(f.Published) == 0 && c.Failed == 0 && c.ScheduledNext == 0 && c.ManualDue == 0 {
		return nil
	}
	p := &models.WeeklyPublishing{ChartHeight: publishingChartPx, CalendarURL: l.to("/calendar")}
	p.Published = models.WeeklyFigure{Value: grouped(len(f.Published))}
	if d, up, ok := percentDelta(float64(len(f.Published)), float64(c.PrevPublished)); ok {
		p.Published.Delta, p.Published.Good = d+" vs last week", up
	}
	if c.Failed > 0 {
		p.Failed = grouped(c.Failed)
		p.FailedNote = failedNote(c.Failed, c.PrevFailed)
	}
	perDay := make([]int, 7)
	for _, post := range f.Published {
		if i := f.Window.dayIndex(post.PublishedAt); i >= 0 {
			perDay[i]++
		}
	}
	p.Bars = bars(perDay, publishingChartPx)
	p.Channels, p.MoreChannels = channels(f)
	p.Failures = failures(f, l)
	if more := c.Failed - len(p.Failures); more > 0 {
		p.MoreFailures = fmt.Sprintf("Review all %d", c.Failed)
	}
	if len(p.Failures) > 0 {
		p.FailuresURL = l.to("/calendar")
	}
	p.NextWeek = nextWeek(c)
	return p
}

func failedNote(cur, prev int) string {
	switch {
	case cur < prev:
		return fmt.Sprintf("%d fewer", prev-cur)
	case cur > prev && prev > 0:
		return fmt.Sprintf("%d more", cur-prev)
	default:
		return ""
	}
}

func nextWeek(c repository.WeeklyCounts) string {
	var parts []string
	if c.ScheduledNext > 0 {
		parts = append(parts, plural(c.ScheduledNext, "post scheduled", "posts scheduled"))
	}
	if c.ManualDue > 0 {
		parts = append(parts, grouped(c.ManualDue)+" to publish by hand")
	}
	return strings.Join(parts, " · ")
}

func bars(values []int, maxPx int) []models.WeeklyBar {
	heights := barHeights(values, maxPx)
	out := make([]models.WeeklyBar, len(values))
	for i, v := range values {
		out[i] = models.WeeklyBar{Day: dayLetters[i%7], Height: heights[i]}
		if v > 0 {
			out[i].Value = grouped(v)
		}
	}
	return out
}

type channelRow struct {
	platform, account string
	published, failed int
	reach             int
	hasReach          bool
}

func channels(f Facts) ([]models.WeeklyChannel, string) {
	byKey := map[string]*channelRow{}
	var order []string
	row := func(accountID, platform, account string) *channelRow {
		key := cmp.Or(accountID, "platform:"+platform)
		r, ok := byKey[key]
		if !ok {
			r = &channelRow{platform: platform, account: account}
			byKey[key] = r
			order = append(order, key)
		}
		return r
	}
	for _, p := range f.Published {
		r := row(p.SocialAccountID, p.Platform, p.AccountName)
		r.published++
		if f.Analytics != nil {
			if reach, ok := f.Analytics.ReachByPost[p.ID]; ok {
				r.reach += reach
				r.hasReach = true
			}
		}
	}
	for _, fl := range f.Failures {
		row(fl.SocialAccountID, fl.Platform, fl.AccountName).failed++
	}
	rows := make([]*channelRow, 0, len(order))
	for _, k := range order {
		rows = append(rows, byKey[k])
	}
	slices.SortStableFunc(rows, func(a, b *channelRow) int {
		return cmp.Or(cmp.Compare(b.published, a.published), cmp.Compare(b.reach, a.reach))
	})
	more := ""
	if len(rows) > maxChannels {
		more = fmt.Sprintf("+%d more", len(rows)-maxChannels)
		rows = rows[:maxChannels]
	}
	out := make([]models.WeeklyChannel, len(rows))
	for i, r := range rows {
		out[i] = models.WeeklyChannel{
			Platform:  r.platform,
			Account:   r.account,
			Published: grouped(r.published),
			Failed:    grouped(r.failed),
			HasFailed: r.failed > 0,
			Reach:     "–",
		}
		if r.hasReach {
			out[i].Reach = count(r.reach)
		}
	}
	return out, more
}

func failures(f Facts, l links) []models.WeeklyFailure {
	n := min(len(f.Failures), maxFailures)
	out := make([]models.WeeklyFailure, n)
	for i, fl := range f.Failures[:n] {
		reason := strings.TrimSpace(fl.Reason)
		if reason == "" {
			reason = "The platform didn't say why."
		}
		out[i] = models.WeeklyFailure{
			Title:  postTitle(fl.Title, fl.Content),
			Meta:   cmp.Or(fl.Platform, "Unknown platform") + " · " + fl.At.In(f.Window.Loc).Format("Mon, Jan 2"),
			Reason: truncate(reason, 200),
			URL:    l.post(fl.PostID, fl.CampaignID),
		}
	}
	return out
}

func renderPerformance(f Facts, l links) *models.WeeklyPerformance {
	a := f.Analytics
	if a == nil || (len(a.ReachByPost) == 0 && !a.HasFollowers) {
		return nil
	}
	p := &models.WeeklyPerformance{URL: l.to("/analytics")}
	p.Reach = figure(count(a.Reach), float64(a.Reach), float64(a.PrevReach))
	p.Interactions = figure(count(a.Interactions), float64(a.Interactions), float64(a.PrevInteractions))
	rate, prevRate := ratePct(a.Interactions, a.Reach), ratePct(a.PrevInteractions, a.PrevReach)
	p.Engagement = models.WeeklyFigure{Value: compact(rate) + "%"}
	if a.Reach > 0 && a.PrevReach > 0 {
		d, up, _ := pointsDelta(rate, prevRate)
		p.Engagement.Delta, p.Engagement.Good = d, up
	}
	if a.HasFollowers {
		p.Followers = models.WeeklyFigure{Value: grouped(a.Followers)}
		if gain := a.Followers - a.FollowersStart; gain != 0 {
			p.Followers.Delta, p.Followers.Good = signed(gain), gain > 0
		}
		if slices.ContainsFunc(a.FollowerGains, func(g int) bool { return g > 0 }) {
			p.FollowerHeight = followerChartPx
			p.FollowerBars = bars(a.FollowerGains, followerChartPx)
		}
	}
	for i, pp := range a.Top {
		p.Top = append(p.Top, perfRow(f, l, pp, fmt.Sprint(i+1)))
	}
	for _, pp := range a.Below {
		p.Below = append(p.Below, perfRow(f, l, pp, ""))
	}
	return p
}

func figure(value string, cur, prev float64) models.WeeklyFigure {
	fig := models.WeeklyFigure{Value: value}
	if d, up, ok := percentDelta(cur, prev); ok {
		fig.Delta, fig.Good = d, up
	}
	return fig
}

func ratePct(interactions, reach int) float64 {
	if reach <= 0 {
		return 0
	}
	return float64(interactions) / float64(reach) * 100
}

func perfRow(f Facts, l links, pp PerfPost, rank string) models.WeeklyPost {
	row := models.WeeklyPost{
		Rank:  rank,
		Title: postTitle(pp.Title, ""),
		Meta:  platformName(pp.Platform) + " · " + pp.PublishedAt.In(f.Window.Loc).Format("Mon, Jan 2"),
		Reach: count(pp.Reach) + " reach",
		URL:   l.post(pp.PostID, campaignOf(f, pp.PostID)),
	}
	if pp.Multiple > 0 {
		row.Multiple = compact(math.Round(pp.Multiple*10)/10) + "× typical"
	}
	return row
}

func campaignOf(f Facts, postID string) string {
	for _, p := range f.Published {
		if p.ID == postID {
			return p.CampaignID
		}
	}
	return ""
}

func renderCampaigns(f Facts, l links) *models.WeeklyCampaigns {
	if len(f.Campaigns) == 0 && f.Counts.CampaignsCreated == 0 {
		return nil
	}
	c := &models.WeeklyCampaigns{URL: l.to("/campaigns")}
	for _, g := range f.Campaigns[:min(len(f.Campaigns), maxCampaigns)] {
		c.Rows = append(c.Rows, campaignRow(g, l))
	}
	if n := f.Counts.CampaignsCreated; n > 0 {
		c.Summary = plural(n, "new campaign", "new campaigns")
	}
	return c
}

func campaignRow(g CampaignGoal, l links) models.WeeklyCampaign {
	unit := "week"
	if g.Cadence == "month" {
		unit = "month"
	}
	goal := fmt.Sprintf("Goal: %s a %s", plural(g.Target, "post", "posts"), unit)
	if g.Streak >= 2 {
		goal += fmt.Sprintf(" · %d-%s streak", g.Streak, unit)
	}
	status := fmt.Sprintf("%d of %d this %s", g.Achieved, g.Target, unit)
	if g.Reached {
		status = "Goal met"
	}
	fill := goalBarPx
	if g.Target > 0 && g.Achieved < g.Target {
		fill = int(math.Round(float64(goalBarPx) * float64(g.Achieved) / float64(g.Target)))
	}
	return models.WeeklyCampaign{
		Name:   g.Name,
		Goal:   goal,
		Status: status,
		Met:    g.Reached,
		Fill:   fill,
		Rest:   goalBarPx - fill,
		URL:    l.to("/campaigns/" + g.ID),
	}
}

func renderIdeas(f Facts, l links) *models.WeeklyIdeas {
	c := f.Counts
	if c.IdeasWaiting == 0 && c.IdeasCreated == 0 && c.AssetsCreated == 0 && c.RemindersDue == 0 {
		return nil
	}
	i := &models.WeeklyIdeas{URL: l.to("/ideas")}
	switch c.IdeasWaiting {
	case 0:
	case 1:
		i.Waiting = "1 idea is waiting for your call"
	default:
		i.Waiting = grouped(c.IdeasWaiting) + " ideas are waiting for your call"
	}
	var parts []string
	if c.IdeasCreated > 0 {
		parts = append(parts, plural(c.IdeasCreated, "new idea", "new ideas"))
	}
	if c.AssetsCreated > 0 {
		parts = append(parts, plural(c.AssetsCreated, "new asset", "new assets"))
	}
	if c.RemindersDue > 0 {
		parts = append(parts, plural(c.RemindersDue, "reminder", "reminders")+" due today")
	}
	i.Summary = strings.Join(parts, " · ")
	return i
}

func renderTeam(f Facts) *models.WeeklyTeam {
	c := f.Counts
	var parts []string
	if f.ActiveMembers >= 0 && c.MembersTotal > 1 {
		parts = append(parts, fmt.Sprintf("%d of %d members were active", f.ActiveMembers, c.MembersTotal))
	}
	if c.MembersJoined > 0 {
		parts = append(parts, plural(c.MembersJoined, "member joined", "members joined"))
	}
	if c.InvitesPending > 0 {
		parts = append(parts, plural(c.InvitesPending, "invite pending", "invites pending"))
	}
	if len(parts) == 0 {
		return nil
	}
	return &models.WeeklyTeam{Summary: strings.Join(parts, " · ")}
}
