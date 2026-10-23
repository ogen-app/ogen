package weeklyreport

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/analytics/performers"
	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
)

// Caps on how many rows each list carries; the rest is summarised as "+N more".
const (
	maxFailures = 5
	// maxFailureRows bounds the failure rows read, which also attribute
	// failures to channels; the email lists only maxFailures of them.
	maxFailureRows = 100
	maxCampaigns   = 5
	maxTop         = 3
	maxBelow       = 2
)

// Store is the main-DB half of the report's reads.
type Store interface {
	Counts(ctx context.Context, tenantID string, w repository.ReportWindow) (repository.WeeklyCounts, error)
	PublishedPosts(ctx context.Context, tenantID string, from, to time.Time) ([]repository.ReportPost, error)
	Failures(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]repository.ReportFailure, error)
	AttentionAccounts(ctx context.Context, tenantID string, expiringBefore time.Time) ([]repository.ReportAccount, error)
	GoalCampaigns(ctx context.Context, tenantID string) ([]repository.ReportCampaign, error)
}

// Analytics reads post metrics from the analytics DB, scoped to the ctx tenant.
type Analytics interface {
	PublishedBetween(ctx context.Context, from, to time.Time) ([]models.PostAnalytics, error)
	ReachByAgeSamples(ctx context.Context) ([]repository.ReachAgeSample, error)
}

// Followers reads the follower time series, scoped to the ctx tenant.
type Followers interface {
	Summary(ctx context.Context, accountID string) ([]repository.FollowerAccountSummary, error)
	Series(ctx context.Context, opts repository.FollowerSeriesOptions) ([]repository.FollowerSeriesPoint, error)
}

// Activity counts the members who were active.
type Activity interface {
	CountDistinctUsers(ctx context.Context, tenantID string, from, to time.Time) (int, error)
}

// CampaignGoals computes a campaign's goal progress.
type CampaignGoals interface {
	Overview(ctx context.Context, campaignID string) (*overview.Overview, error)
}

// Limits reports live plan usage.
type Limits interface {
	Usage(ctx context.Context, tenantID string) ([]entitlements.FeatureUsage, error)
}

// Facts is everything one week's report is made of, before formatting.
type Facts struct {
	Window    Window
	Workspace string
	Counts    repository.WeeklyCounts
	Published []repository.ReportPost
	Failures  []repository.ReportFailure
	Attention []repository.ReportAccount
	Limits    []entitlements.FeatureUsage
	Campaigns []CampaignGoal
	// Analytics is nil when the analytics DB is unavailable.
	Analytics *AnalyticsFacts
	// ActiveMembers is -1 when it couldn't be counted.
	ActiveMembers int
	// SectionErrors names the optional sections that failed and were dropped.
	SectionErrors []string
}

// CampaignGoal is one campaign's progress in the goal period the week ends in.
type CampaignGoal struct {
	ID       string
	Name     string
	Cadence  string
	Target   int
	Achieved int
	Reached  bool
	Streak   int
}

// AnalyticsFacts are the week's post metrics, using the same definitions as the
// in-app overview: lifetime metrics of the posts published in the window, and
// interactions = likes + comments + shares.
type AnalyticsFacts struct {
	ReachByPost      map[string]int
	Reach            int
	PrevReach        int
	Interactions     int
	PrevInteractions int
	HasFollowers     bool
	Followers        int
	FollowersStart   int
	FollowerGains    []int
	Top              []PerfPost
	Below            []PerfPost
}

// PerfPost is a post scored against the workspace's typical post. Multiple is
// 0 when there isn't enough history to score it.
type PerfPost struct {
	PostID      string
	Title       string
	Platform    string
	Reach       int
	Multiple    float64
	PublishedAt time.Time
}

// Collector gathers Facts. Store is required; every other dependency is
// optional, and a nil or failing one only drops its section.
type Collector struct {
	Store     Store
	Analytics Analytics
	Followers Followers
	Activity  Activity
	Goals     CampaignGoals
	Limits    Limits
	// LimitPct is the usage share from which a plan limit is flagged.
	LimitPct int
	// ExpiryLead flags a connection whose token expires within this long.
	ExpiryLead time.Duration
}

const collectComp = "weeklyreport.collect"

// Collect gathers one workspace's week. ctx must be scoped to tenantID. A
// main-DB error fails the whole report so the next dispatcher tick retries it.
func (c *Collector) Collect(ctx context.Context, tenantID, workspace string, w Window) (Facts, error) {
	f := Facts{Window: w, Workspace: workspace, ActiveMembers: -1}
	var err error
	if f.Counts, err = c.Store.Counts(ctx, tenantID, w.repo()); err != nil {
		return f, err
	}
	if f.Published, err = c.Store.PublishedPosts(ctx, tenantID, w.Start, w.End); err != nil {
		return f, err
	}
	if f.Failures, err = c.Store.Failures(ctx, tenantID, w.Start, w.End, maxFailureRows); err != nil {
		return f, err
	}
	if f.Attention, err = c.Store.AttentionAccounts(ctx, tenantID, w.Now.Add(c.ExpiryLead)); err != nil {
		return f, err
	}
	c.collectOptional(ctx, tenantID, &f)
	return f, nil
}

// collectOptional fills the sections whose failure only drops that section.
func (c *Collector) collectOptional(ctx context.Context, tenantID string, f *Facts) {
	sections := []struct {
		name string
		run  func() error
	}{
		{"campaigns", func() (err error) { f.Campaigns, err = c.campaignGoals(ctx, tenantID, f.Window); return err }},
		{"performance", func() (err error) { f.Analytics, err = c.analytics(ctx, f.Window); return err }},
		{"limits", func() (err error) { f.Limits, err = c.nearLimits(ctx, tenantID); return err }},
		{"team", func() error { return c.activeMembers(ctx, tenantID, f) }},
	}
	for _, s := range sections {
		if err := s.run(); err != nil {
			f.SectionErrors = append(f.SectionErrors, s.name)
			slog.WarnContext(ctx, "weekly report section dropped", logging.AttrComponent, collectComp,
				"tenant_id", tenantID, "section", s.name, logging.AttrError, err)
		}
	}
}

func (c *Collector) activeMembers(ctx context.Context, tenantID string, f *Facts) error {
	if c.Activity == nil {
		return nil
	}
	n, err := c.Activity.CountDistinctUsers(ctx, tenantID, f.Window.Start, f.Window.End)
	if err != nil {
		return err
	}
	f.ActiveMembers = n
	return nil
}

func (c *Collector) nearLimits(ctx context.Context, tenantID string) ([]entitlements.FeatureUsage, error) {
	if c.Limits == nil || c.LimitPct <= 0 {
		return nil, nil
	}
	usage, err := c.Limits.Usage(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var out []entitlements.FeatureUsage
	for _, u := range usage {
		if u.Limit > 0 && u.Current*100 >= u.Limit*int64(c.LimitPct) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (c *Collector) campaignGoals(ctx context.Context, tenantID string, w Window) ([]CampaignGoal, error) {
	if c.Goals == nil {
		return nil, nil
	}
	camps, err := c.Store.GoalCampaigns(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// The goal period that holds the last moment of the reported week.
	ref := w.End.Add(-time.Second)
	var out []CampaignGoal
	for _, cp := range camps {
		ov, err := c.Goals.Overview(ctx, cp.ID)
		if err != nil {
			return nil, err
		}
		if g, ok := goalAt(ov.Goal, ref); ok {
			g.ID, g.Name = cp.ID, cp.Name
			out = append(out, g)
		}
	}
	return out, nil
}

// goalAt picks the goal period containing ref and the run of reached periods
// ending with it.
func goalAt(gp *overview.GoalProgress, ref time.Time) (CampaignGoal, bool) {
	if gp == nil {
		return CampaignGoal{}, false
	}
	for i, b := range gp.Buckets {
		if ref.Before(b.Start) || !ref.Before(b.End) {
			continue
		}
		streak := 0
		for j := i; j >= 0 && gp.Buckets[j].Reached; j-- {
			streak++
		}
		return CampaignGoal{
			Cadence:  gp.Cadence,
			Target:   b.Target,
			Achieved: b.Achieved,
			Reached:  b.Reached,
			Streak:   streak,
		}, true
	}
	return CampaignGoal{}, false
}

func (c *Collector) analytics(ctx context.Context, w Window) (*AnalyticsFacts, error) {
	if c.Analytics == nil {
		return nil, nil
	}
	cur, err := c.Analytics.PublishedBetween(ctx, w.Start, w.End)
	if err != nil {
		return nil, err
	}
	prev, err := c.Analytics.PublishedBetween(ctx, w.PrevStart, w.Start)
	if err != nil {
		return nil, err
	}
	a := &AnalyticsFacts{ReachByPost: make(map[string]int, len(cur))}
	for i := range cur {
		a.ReachByPost[cur[i].PostID] = cur[i].Reach
		a.Reach += cur[i].Reach
		a.Interactions += interactions(&cur[i])
	}
	for i := range prev {
		a.PrevReach += prev[i].Reach
		a.PrevInteractions += interactions(&prev[i])
	}
	if err := c.followers(ctx, w, a); err != nil {
		return nil, err
	}
	if len(cur) > 0 {
		samples, err := c.Analytics.ReachByAgeSamples(ctx)
		if err != nil {
			return nil, err
		}
		a.Top, a.Below = rankPosts(cur, samples, w.Now)
	}
	return a, nil
}

func interactions(r *models.PostAnalytics) int { return r.Likes + r.Comments + r.Shares }

func (c *Collector) followers(ctx context.Context, w Window, a *AnalyticsFacts) error {
	if c.Followers == nil {
		return nil
	}
	summary, err := c.Followers.Summary(ctx, "")
	if err != nil {
		return err
	}
	if len(summary) == 0 {
		return nil
	}
	for _, s := range summary {
		a.Followers += int(s.CurrentFollowers)
	}
	a.HasFollowers = true
	pts, err := c.Followers.Series(ctx, repository.FollowerSeriesOptions{From: w.Start.AddDate(0, 0, -1), To: w.End})
	if err != nil {
		return err
	}
	totals := map[string]int{}
	for _, p := range pts {
		totals[p.PointDate.UTC().Format(time.DateOnly)] += int(p.Followers)
	}
	a.FollowersStart = a.Followers
	before := dateKey(w.Start.AddDate(0, 0, -1))
	if t, ok := totals[before]; ok {
		a.FollowersStart = t
	}
	a.FollowerGains = make([]int, 7)
	for i, d := range w.Days() {
		today, okT := totals[dateKey(d)]
		yesterday, okY := totals[dateKey(d.AddDate(0, 0, -1))]
		if okT && okY && today > yesterday {
			a.FollowerGains[i] = today - yesterday
		}
	}
	return nil
}

// dateKey is the calendar date of a local midnight, matching the follower
// series' point_date.
func dateKey(t time.Time) string { return t.Format(time.DateOnly) }

// rankPosts scores the week's posts against the workspace's typical post for
// the platform and age, returning the best few and the clearly-below few.
func rankPosts(cur []models.PostAnalytics, samples []repository.ReachAgeSample, now time.Time) (top, below []PerfPost) {
	cands := make([]performers.Candidate, 0, len(cur))
	for i := range cur {
		r := cur[i]
		if r.PublishedAt == nil {
			continue
		}
		cands = append(cands, performers.Candidate{
			PostID:         r.PostID,
			Title:          r.Title,
			Platform:       r.Platform,
			Reach:          r.Reach,
			Impressions:    r.Impressions,
			Likes:          r.Likes,
			Comments:       r.Comments,
			Shares:         r.Shares,
			EngagementRate: r.EngagementRate,
			PublishedAt:    *r.PublishedAt,
			AgeDays:        max(0, int(now.Sub(*r.PublishedAt)/(24*time.Hour))),
		})
	}
	ps := make([]performers.Sample, len(samples))
	for i, s := range samples {
		ps[i] = performers.Sample{Platform: s.Platform, PostID: s.PostID, AgeDay: s.AgeDay, Reach: s.Reach, Interactions: s.Interactions}
	}
	res := performers.Build(cands, ps, performers.Options{By: performers.ByAgainstTypical, Limit: maxTop})
	for _, row := range res.Best {
		top = append(top, perfPost(row))
	}
	for _, row := range res.Worst {
		if row.Direction == "below" && len(below) < maxBelow {
			below = append(below, perfPost(row))
		}
	}
	return top, below
}

func perfPost(row performers.Row) PerfPost {
	p := PerfPost{PostID: row.PostID, Title: row.Title, Platform: row.Platform, Reach: row.Reach, PublishedAt: row.PublishedAt}
	if row.AgainstTypical != nil {
		p.Multiple = *row.AgainstTypical
	}
	return p
}

// Empty reports a week with nothing worth mailing. Attention items alone don't
// count: the connection-expiry email already covers them.
func (f Facts) Empty() bool {
	c := f.Counts
	return len(f.Published) == 0 && c.Failed == 0 && c.ScheduledNext == 0 && c.ManualDue == 0 &&
		c.PostsCreated == 0 && c.IdeasCreated == 0 && c.AssetsCreated == 0 &&
		c.CampaignsCreated == 0 && c.MembersJoined == 0
}
