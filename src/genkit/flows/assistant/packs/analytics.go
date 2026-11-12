package packs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/analytics/learnings"
	"github.com/ogen-app/ogen/src/analytics/performers"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

const analyticsPackName = "analytics"

// maxExamples caps the posts "more like these" learns from.
const maxExamples = 5

// analyticsPack explains how posts did, over the window on screen, and plans
// ideas like what worked from the ideas and assets behind it.
func analyticsPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    analyticsPackName,
		Summary: "say how posts performed over a window — totals, trends, best and worst posts, two periods side by side — or one post on its own; the best times, cadence, decay and follower growth; what the workspace's history has taught; set the dashboard's window; and write ideas like the posts that worked",
		Screens: []viewcontext.Screen{"analytics", "analytics.post", "campaign.analytics"},
		// Each read's last output reaches the client as
		// complete.results.analytics.<tool>, with the filters it applied, so
		// cards cite them and replay from history.
		Begin: beginResults(analyticsPackName),
		End:   endResults(analyticsPackName),
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			overview := genkit.DefineTool(g, "analyticsOverview",
				"Returns the analytics overview for a window: reach, interactions, posts published and followers, against the previous window, with trends and insights.",
				assistantkit.Wrap(recorded(analyticsPackName, "analyticsOverview", nil, func(ctx context.Context, in AnalyticsQuery) (*AnalyticsResult, error) {
					return analyticsOverview(ctx, d, in)
				})), ai.WithStrictSchema(false))
			top := genkit.DefineTool(g, "topPosts",
				"Returns a window's best and worst posts, scored against the workspace's typical post at the same age, with insights.",
				assistantkit.Wrap(recorded(analyticsPackName, "topPosts", nil, func(ctx context.Context, in TopPostsInput) (*AnalyticsResult, error) {
					return topPosts(ctx, d, in)
				})), ai.WithStrictSchema(false))
			lessons := genkit.DefineTool(g, "analyticsLessons",
				"Returns what the workspace's whole history has taught: the best days and hours to post, how long a post keeps gaining reach, and the structural patterns that work or are fading.",
				assistantkit.Wrap(recorded(analyticsPackName, "analyticsLessons", nil, func(ctx context.Context, in LessonsInput) (*AnalyticsResult, error) {
					return analyticsLessons(ctx, d, in)
				})), ai.WithStrictSchema(false))
			stats := genkit.DefineTool(g, "postStats",
				"Returns one published post's statistics: reach, impressions, interactions, saves, clicks and engagement rate against the workspace's typical post at the same age, their running totals since publishing, and a narrative.",
				assistantkit.Wrap(recorded(analyticsPackName, "postStats", nil, func(ctx context.Context, in PostStatsInput) (*AnalyticsResult, error) {
					return postStats(ctx, d, in)
				})), ai.WithStrictSchema(false))
			specs := []assistantkit.Spec{
				spec(analyticsPackName, overview, assistantkit.TierRead,
					`Call this when the user asks how things are going — "how did we do this month?", "is reach up?". The window defaults to the one on screen, else the last 28 days; the campaign to the thread's. Quote numbers as given; don't compute your own.`),
				spec(analyticsPackName, top, assistantkit.TierRead,
					`Call this for the best or worst posts — "what worked last month?", "which posts underperformed?". by is against_typical (default), reach, engagement_rate or interactions.`),
				spec(analyticsPackName, lessons, assistantkit.TierRead,
					`Call this for what has worked over time — "when should we post?", "what kind of posts do best?", "how long does a post keep going?". Lessons are all-time and workspace-wide, never one campaign's; platforms default to the screen's. A section marked insufficient_history has too little data: say so rather than guess.`),
				spec(analyticsPackName, stats, assistantkit.TierRead,
					`Call this when the user asks how one post did — "how is this post doing?", "why did that one flop?". postId defaults to the post the thread is about; take ids from topPosts otherwise. Quote the multipliers and bands as given.`),
			}
			specs = append(specs, analyticsReadSpecs(g, d)...)
			specs = append(specs, comparePeriodsSpec(g, d), setWindowSpec(g, d))
			if d.MoreLike != nil && d.IdeaRepo != nil && d.Links != nil {
				specs = append(specs, moreLikeTheseSpec(g, d))
			}
			return specs, nil
		},
	}
}

// AnalyticsQuery narrows an analytics read.
type AnalyticsQuery struct {
	Window     string   `json:"window,omitempty"     jsonschema:"description=Relative window like 7d, 28d, 12w, 6mo; ignored with from and to"`
	From       string   `json:"from,omitempty"       jsonschema:"description=Inclusive start, YYYY-MM-DD"`
	To         string   `json:"to,omitempty"         jsonschema:"description=Inclusive end, YYYY-MM-DD"`
	Platforms  []string `json:"platforms,omitempty"  jsonschema:"description=Platform slugs, e.g. linkedin, instagram"`
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Defaults to the campaign on screen, else the thread's; 'all' for the whole workspace"`
}

// rangeAndScope resolves a query against the screen: its window when the
// query names no dates, its platforms when the query names none, and its
// campaign, else the thread's, unless the query names one or "all".
func rangeAndScope(ctx context.Context, d Deps, in AnalyticsQuery, granularity string) (timeframe.Range, *analytics.Scope, error) {
	from, to, window, platforms := in.From, in.To, in.Window, in.Platforms
	if w := assistant.Window(ctx); w != nil {
		if from == "" && to == "" && window == "" {
			from, to = w.From, w.To
		}
		if len(platforms) == 0 {
			platforms = w.Platforms
		}
	}
	rng, err := timeframe.Resolve(from, to, window, granularity, time.Now().UTC())
	if errors.Is(err, timeframe.ErrWindowTooLarge) {
		return rng, nil, assistantkit.Refused("that window is too long; pick a shorter one")
	}
	if err != nil {
		return rng, nil, assistantkit.Refused("the window is a relative span like 28d, or from and to dates")
	}
	campaign := in.CampaignID
	switch {
	case strings.EqualFold(campaign, "all"):
		campaign = ""
	case campaign == "" && assistant.Window(ctx) != nil && assistant.Window(ctx).CampaignID != "":
		campaign = assistant.Window(ctx).CampaignID
	case campaign == "":
		campaign = assistant.CampaignID(ctx)
	}
	sc, err := resolveScope(ctx, d, platforms, campaign)
	return rng, sc, err
}

// defaultPreset is the span a read with no dates and no window covers.
const defaultPreset = "28d"

// presetOf is the relative span the read's dates resolve, as rangeAndScope
// picks them: the call's dates are none, its window that span, the
// screen's dates its preset, and none at all the default.
func presetOf(ctx context.Context, in AnalyticsQuery) string {
	switch {
	case in.From != "" || in.To != "":
		return ""
	case in.Window != "":
		return viewcontext.CanonicalPreset(in.Window)
	}
	if w := assistant.Window(ctx); w != nil && (w.From != "" || w.To != "") {
		return w.Preset
	}
	return defaultPreset
}

// appliedFor is a windowed read's filters, with the span its dates
// resolve.
func appliedFor(ctx context.Context, in AnalyticsQuery, rng timeframe.Range, sc *analytics.Scope, note string) Applied {
	a := windowApplied(rng, sc, note)
	a.Preset = presetOf(ctx, in)
	return a
}

// resolveScope narrows a read to platforms and a campaign, refusing unknown
// ones.
func resolveScope(ctx context.Context, d Deps, platforms []string, campaign string) (*analytics.Scope, error) {
	sc, err := d.Analytics.Scope(ctx, platforms, campaign)
	switch {
	case errors.Is(err, analytics.ErrInvalidPlatform):
		return nil, assistantkit.Refused("one of those platforms isn't known; use slugs like linkedin or instagram")
	case errors.Is(err, analytics.ErrCampaignNotFound):
		return nil, assistantkit.Refused("that campaign doesn't exist")
	}
	return sc, err
}

// Unavailable is a read with no data, and why.
type Unavailable struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// AnalyticsResult is a read's data and what it was narrowed to.
type AnalyticsResult struct {
	Applied Applied `json:"applied"`
	// Data is the read, or Unavailable when it has none.
	Data any `json:"data"`
	// Summary is what the read's card leads with, worked out from Data and
	// beside it, so Data keeps the dashboard endpoint's shape.
	Summary any `json:"summary,omitempty"`
}

// Applied is what a read was narrowed to, which its card cites: the days
// (inclusive), the platforms (empty for every one), the campaign (nil for
// the whole workspace), or the one post it is about.
type Applied struct {
	From      string           `json:"from,omitempty"`
	To        string           `json:"to,omitempty"`
	Platforms []string         `json:"platforms,omitempty"`
	Campaign  *AppliedCampaign `json:"campaign,omitempty"`
	PostID    string           `json:"postId,omitempty"`
	// Preset is the relative span the dates resolve, e.g. 30d, so a card
	// says "Last 30 days" as the period picker does; empty for dates.
	Preset string `json:"preset,omitempty"`
	// Note says what the read can't be narrowed by.
	Note string `json:"note,omitempty"`
}

// AppliedCampaign is the campaign a read was narrowed to.
type AppliedCampaign struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// windowApplied is a windowed read's filters.
func windowApplied(rng timeframe.Range, sc *analytics.Scope, note string) Applied {
	a := Applied{From: rng.From.Format(time.DateOnly), To: rng.To.AddDate(0, 0, -1).Format(time.DateOnly), Platforms: sc.Platforms(), Note: note}
	if id := sc.Campaign(); id != "" {
		a.Campaign = &AppliedCampaign{ID: id, Name: sc.CampaignName()}
	}
	return a
}

// result is a read's data with its filters; data nil is Unavailable for
// reason.
func result[T any](a Applied, data *T, reason string) *AnalyticsResult {
	if data == nil {
		return &AnalyticsResult{Applied: a, Data: Unavailable{Reason: reason}}
	}
	return &AnalyticsResult{Applied: a, Data: data}
}

func analyticsOverview(ctx context.Context, d Deps, in AnalyticsQuery) (*AnalyticsResult, error) {
	rng, sc, err := rangeAndScope(ctx, d, in, "")
	if err != nil {
		return nil, err
	}
	resp, reason, err := d.Analytics.Overview(ctx, rng, sc)
	if err != nil {
		return nil, err
	}
	return result(appliedFor(ctx, in, rng, sc, ""), resp, reason), nil
}

// TopPostsInput ranks a window's posts.
type TopPostsInput struct {
	AnalyticsQuery
	By    string `json:"by,omitempty"    jsonschema:"description=against_typical (default), reach, engagement_rate or interactions"`
	Limit int    `json:"limit,omitempty" jsonschema:"description=Posts per list; default 5"`
}

func topPosts(ctx context.Context, d Deps, in TopPostsInput) (*AnalyticsResult, error) {
	by := in.By
	if by == "" {
		by = performers.ByAgainstTypical
	}
	if !performers.ValidBy(by) {
		return nil, assistantkit.Refused("by is against_typical, reach, engagement_rate or interactions")
	}
	limit := performers.DefaultLimit
	if in.Limit > 0 {
		limit = min(in.Limit, 10)
	}
	rng, sc, err := rangeAndScope(ctx, d, in.AnalyticsQuery, "")
	if err != nil {
		return nil, err
	}
	res, reason, err := d.Analytics.Performers(ctx, rng, sc, by, limit)
	if err != nil {
		return nil, err
	}
	return result(appliedFor(ctx, in.AnalyticsQuery, rng, sc, ""), res, reason), nil
}

// LessonsInput shapes the lessons read.
type LessonsInput struct {
	Since       string   `json:"since,omitempty"       jsonschema:"description=Learn only from posts published on or after this day, YYYY-MM-DD; default all time"`
	TrendWindow string   `json:"trendWindow,omitempty" jsonschema:"description=Span the fading comparison looks back over, like 90d, 12w or 3mo; default 90d"`
	Metric      string   `json:"metric,omitempty"      jsonschema:"description=reach (default) or saves"`
	Platforms   []string `json:"platforms,omitempty"   jsonschema:"description=Platform slugs, e.g. linkedin, instagram"`
}

func analyticsLessons(ctx context.Context, d Deps, in LessonsInput) (*AnalyticsResult, error) {
	opt := analytics.LessonsOptions{Metric: in.Metric}
	if opt.Metric == "" {
		opt.Metric = learnings.MetricReach
	}
	if opt.Metric != learnings.MetricReach && opt.Metric != learnings.MetricSaves {
		return nil, assistantkit.Refused("metric is reach or saves")
	}
	if in.Since != "" {
		t, err := time.Parse(time.DateOnly, in.Since)
		if err != nil {
			return nil, assistantkit.Refused("since is a day, YYYY-MM-DD")
		}
		opt.Since = &t
	}
	var ok bool
	if opt.TrendWindowDays, ok = analytics.ParseTrendWindow(in.TrendWindow); !ok {
		return nil, assistantkit.Refused("trendWindow is a span like 90d, 12w or 3mo")
	}
	platforms := in.Platforms
	if w := assistant.Window(ctx); w != nil && len(platforms) == 0 {
		platforms = w.Platforms
	}
	// Lessons are workspace-wide even in a campaign thread: one campaign's
	// history is too thin to learn from.
	sc, err := resolveScope(ctx, d, platforms, "")
	if err != nil {
		return nil, err
	}
	resp, reason, err := d.Analytics.Lessons(ctx, sc, opt)
	if err != nil {
		return nil, err
	}
	a := Applied{From: in.Since, Platforms: sc.Platforms(), Note: "all time unless from is set, and workspace-wide"}
	return result(a, resp, reason), nil
}

// PostStatsInput names the post to read.
type PostStatsInput struct {
	PostID string `json:"postId,omitempty" jsonschema:"description=The post; defaults to the post the thread is about"`
}

func postStats(ctx context.Context, d Deps, in PostStatsInput) (*AnalyticsResult, error) {
	id := strings.TrimSpace(in.PostID)
	if s, ok := assistant.Scope(ctx); ok && id == "" && s.Kind == models.AssistantScopePost {
		id = s.ID
	}
	if id == "" {
		return nil, assistantkit.Refused("pass the post's postId, e.g. from topPosts")
	}
	resp, reason, err := d.Analytics.PostStats(ctx, id)
	switch {
	case errors.Is(err, analytics.ErrPostNotFound):
		return nil, assistantkit.Refused(fmt.Sprintf("there is no post %q", id))
	case errors.Is(err, analytics.ErrNotPublishedViaPublisher):
		return nil, assistantkit.Refused("that post hasn't gone out through a publisher, so it has no statistics")
	case err != nil:
		return nil, err
	}
	return result(Applied{PostID: id}, resp, reason), nil
}
