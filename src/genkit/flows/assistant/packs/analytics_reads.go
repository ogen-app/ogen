package packs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// InsightInput narrows a live insight read from the publisher.
type InsightInput struct {
	Platform  string `json:"platform,omitempty"  jsonschema:"description=One platform slug, e.g. linkedin; defaults to the screen's when it shows exactly one, else every platform"`
	AccountID string `json:"accountId,omitempty" jsonschema:"description=One connected account's id"`
	Source    string `json:"source,omitempty"    jsonschema:"description=all (default), late (posts published through Ogen) or external"`
}

// FollowerHistoryInput narrows the follower history.
type FollowerHistoryInput struct {
	Window      string `json:"window,omitempty"      jsonschema:"description=Relative window like 28d, 12w, 6mo; ignored with from and to"`
	From        string `json:"from,omitempty"        jsonschema:"description=Inclusive start, YYYY-MM-DD"`
	To          string `json:"to,omitempty"          jsonschema:"description=Inclusive end, YYYY-MM-DD"`
	AccountID   string `json:"accountId,omitempty"   jsonschema:"description=One connected account's id"`
	Granularity string `json:"granularity,omitempty" jsonschema:"description=daily (default), weekly or monthly"`
}

// ListPostAnalyticsInput pages the published posts' figures.
type ListPostAnalyticsInput struct {
	SortBy   string `json:"sortBy,omitempty"   jsonschema:"description=engagement (default), impressions, reach, likes, comments, shares, saves, clicks, views or published_at"`
	Order    string `json:"order,omitempty"    jsonschema:"description=desc (default) or asc"`
	Platform string `json:"platform,omitempty" jsonschema:"description=One platform slug, e.g. linkedin"`
	Limit    int    `json:"limit,omitempty"    jsonschema:"description=Posts per page; default 10, at most 25"`
	Page     int    `json:"page,omitempty"     jsonschema:"description=Page, from 1"`
}

// postSorts are the posts list's sort keys.
var postSorts = []string{"engagement", "impressions", "reach", "likes", "comments", "shares", "saves", "clicks", "views", "published_at"}

// analyticsReadSpecs are the reads behind the analytics screens' other
// panels: the publisher's best times, decay and posting frequency, the
// follower history, and the posts list.
func analyticsReadSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	bestTimes := genkit.DefineTool(g, "bestTimes",
		"Returns the publisher's best times to post: average interactions per post by day of week and hour (UTC) across the workspace's connected accounts, including posts published outside Ogen, with the best slot, the best on another day and the weakest.",
		assistantkit.Wrap(recorded(analyticsPackName, "bestTimes", nil, func(ctx context.Context, in InsightInput) (*AnalyticsResult, error) {
			return bestTimesRead(ctx, d, in)
		})), ai.WithStrictSchema(false))
	decay := genkit.DefineTool(g, "contentDecay",
		"Returns how a post's engagement accrues over time from the publisher: what share of its final engagement it has reached by each age, and how many hours it takes to reach half, overall and per connected platform.",
		assistantkit.Wrap(recorded(analyticsPackName, "contentDecay", nil, func(ctx context.Context, in InsightInput) (*AnalyticsResult, error) {
			return decayRead(ctx, d, in)
		})), ai.WithStrictSchema(false))
	frequency := genkit.DefineTool(g, "postingFrequency",
		"Returns how often the workspace actually posts — posts each week for the last 12 weeks, the weekly average, last month's count and each platform's average, from the posts published through Ogen — and the publisher's engagement rate at each posting cadence (posts per week), per platform.",
		assistantkit.Wrap(recorded(analyticsPackName, "postingFrequency", nil, func(ctx context.Context, in InsightInput) (*AnalyticsResult, error) {
			return frequencyRead(ctx, d, in, time.Now())
		})), ai.WithStrictSchema(false))
	followers := genkit.DefineTool(g, "followerHistory",
		"Returns each connected account's follower count over a window, with its growth, from the daily snapshots.",
		assistantkit.Wrap(recorded(analyticsPackName, "followerHistory", nil, func(ctx context.Context, in FollowerHistoryInput) (*AnalyticsResult, error) {
			return followerHistory(ctx, d, in)
		})),
		ai.WithStrictSchema(false))
	posts := genkit.DefineTool(g, "listPostAnalytics",
		"Lists the posts published through Ogen with their figures, sorted by one metric, a page at a time, with totals over all of them.",
		assistantkit.Wrap(recorded(analyticsPackName, "listPostAnalytics", nil, func(ctx context.Context, in ListPostAnalyticsInput) (*AnalyticsResult, error) {
			return listPostAnalytics(ctx, d, in)
		})),
		ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(analyticsPackName, bestTimes, assistantkit.TierRead,
			`Call this for when to post by the publisher's data — "what's the best time to post on Instagram?". It spans every connected account, posts from outside Ogen too; analyticsLessons' heatmap is Ogen's own history. Lead with summary.best, then summary.runnerUp; the figures are average interactions per post, not a rate. summary.tooFew: say there are too few posts to say yet. A result marked unavailable (addon_required, not_configured): say so.`),
		spec(analyticsPackName, decay, assistantkit.TierRead,
			`Call this for how fast posts gain their engagement — "how quickly do our posts peak?", "is a post done after a day?". Quote summary.halfLife and each platform's as given.`),
		spec(analyticsPackName, frequency, assistantkit.TierRead,
			`Call this for how often the workspace posts and how often to post — "how often do we post?", "do we post too much?", "what cadence works on LinkedIn?". summary.cadence is what was published through Ogen; the rows are the publisher's rate at each cadence. Quote the cadence and rates as given.`),
		spec(analyticsPackName, followers, assistantkit.TierRead,
			`Call this for follower growth over time — "how have followers grown since March?", "which account grew most?". The overview has only the current count.`),
		spec(analyticsPackName, posts, assistantkit.TierRead,
			`Call this to list posts by a metric — "list our posts by saves", "which posts got the most clicks?". For best and worst against the typical post, use topPosts.`),
	}
}

// insightPlatform is the one platform a live insight read takes: the
// input's, else the screen's when it shows exactly one.
func insightPlatform(ctx context.Context, d Deps, slug string) (string, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if w := assistant.Window(ctx); slug == "" && w != nil && len(w.Platforms) == 1 {
		slug = w.Platforms[0]
	}
	if slug == "" {
		return "", nil
	}
	if _, err := d.Analytics.PlatformName(ctx, slug); err != nil {
		if errors.Is(err, analytics.ErrInvalidPlatform) {
			return "", assistantkit.Refused("that platform isn't known; use a slug like linkedin or instagram")
		}
		return "", err
	}
	return slug, nil
}

// insightNote is what a live insight read can't be narrowed by.
const insightNote = "the publisher's figures over every connected account and all time; not narrowed by dates or campaign"

func bestTimesRead(ctx context.Context, d Deps, in InsightInput) (*AnalyticsResult, error) {
	out, err := insightRead(ctx, d, in, d.Analytics.BestTimes)
	if err != nil {
		return nil, err
	}
	if slots, ok := out.Data.([]zernio.BestTimeSlot); ok {
		out.Summary = analytics.SummarizeBestTimes(slots)
	}
	return out, nil
}

// DecaySummary is the half-life overall and, when the read spans several
// connected platforms, on each.
type DecaySummary struct {
	HalfLife  *analytics.HalfLife `json:"halfLife,omitempty"`
	Platforms []PlatformDecay     `json:"platforms,omitempty"`
}

// PlatformDecay is one platform's decay curve and half-life, by its slug.
type PlatformDecay struct {
	Platform string                      `json:"platform"`
	HalfLife *analytics.HalfLife         `json:"halfLife,omitempty"`
	Buckets  []zernio.ContentDecayBucket `json:"buckets"`
}

// decayRead is the decay curve with its half-life, and, unless narrowed to
// one platform or account, each connected platform's. A platform the
// publisher fails on is left out.
func decayRead(ctx context.Context, d Deps, in InsightInput) (*AnalyticsResult, error) {
	out, err := insightRead(ctx, d, in, d.Analytics.ContentDecay)
	if err != nil {
		return nil, err
	}
	buckets, ok := out.Data.([]zernio.ContentDecayBucket)
	if !ok {
		return out, nil
	}
	sum := DecaySummary{HalfLife: analytics.DecayHalfLife(buckets)}
	out.Summary = &sum
	if len(out.Applied.Platforms) > 0 || in.AccountID != "" {
		return out, nil
	}
	slugs, err := d.Analytics.ConnectedPlatforms(ctx)
	if err != nil || len(slugs) < 2 {
		return out, err
	}
	per := make([]*PlatformDecay, len(slugs))
	var wg sync.WaitGroup
	for i, slug := range slugs {
		wg.Go(func() {
			b, reason, err := d.Analytics.ContentDecay(ctx, analytics.InsightFilter{Platform: slug, Source: in.Source})
			if err != nil || reason != "" || len(b) == 0 {
				return
			}
			per[i] = &PlatformDecay{Platform: slug, HalfLife: analytics.DecayHalfLife(b), Buckets: b}
		})
	}
	wg.Wait()
	for _, p := range per {
		if p != nil {
			sum.Platforms = append(sum.Platforms, *p)
		}
	}
	return out, nil
}

// FrequencySummary is how often the workspace posts, from the posts
// published through Ogen.
type FrequencySummary struct {
	Cadence *analytics.PostingCadence `json:"cadence"`
	// Note says what the cadence counts.
	Note string `json:"note"`
}

// frequencyRead is the publisher's rate at each cadence, with the cadence
// the workspace keeps, as of now, on the same platform.
func frequencyRead(ctx context.Context, d Deps, in InsightInput, now time.Time) (*AnalyticsResult, error) {
	out, err := insightRead(ctx, d, in, d.Analytics.PostingFrequency)
	if err != nil {
		return nil, err
	}
	name := ""
	if len(out.Applied.Platforms) == 1 {
		if name, err = d.Analytics.PlatformName(ctx, out.Applied.Platforms[0]); err != nil {
			return nil, err
		}
	}
	cadence, reason, err := d.Analytics.PostingCadence(ctx, name, now)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		out.Summary = &FrequencySummary{Cadence: cadence, Note: "posts published through Ogen, by week starting Monday, UTC"}
	}
	return out, nil
}

func insightRead[T any](ctx context.Context, d Deps, in InsightInput, read func(context.Context, analytics.InsightFilter) (T, string, error)) (*AnalyticsResult, error) {
	if in.Source != "" && in.Source != "all" && in.Source != "late" && in.Source != "external" {
		return nil, assistantkit.Refused("source is all, late or external")
	}
	platform, err := insightPlatform(ctx, d, in.Platform)
	if err != nil {
		return nil, err
	}
	a := Applied{Note: insightNote}
	if platform != "" {
		a.Platforms = []string{platform}
	}
	data, reason, err := read(ctx, analytics.InsightFilter{Platform: platform, AccountID: in.AccountID, Source: in.Source})
	if errors.Is(err, analytics.ErrUpstream) {
		return &AnalyticsResult{Applied: a, Data: Unavailable{Reason: "the publisher's analytics didn't answer; try again shortly"}}, nil
	}
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return &AnalyticsResult{Applied: a, Data: Unavailable{Reason: reason}}, nil
	}
	return &AnalyticsResult{Applied: a, Data: data}, nil
}

func followerHistory(ctx context.Context, d Deps, in FollowerHistoryInput) (*AnalyticsResult, error) {
	granularity := in.Granularity
	if granularity == "" {
		granularity = analytics.GranularityDaily
	}
	if !analytics.ValidGranularity(granularity) {
		return nil, assistantkit.Refused("granularity is daily, weekly or monthly")
	}
	q := AnalyticsQuery{Window: in.Window, From: in.From, To: in.To, CampaignID: "all"}
	rng, _, err := rangeAndScope(ctx, d, q, "")
	if err != nil {
		return nil, err
	}
	res, reason, err := d.Analytics.FollowerHistory(ctx, analytics.FollowerHistoryOptions{
		AccountID: in.AccountID, From: rng.From, To: rng.To.AddDate(0, 0, -1), Granularity: granularity,
	})
	if err != nil {
		return nil, err
	}
	return result(appliedFor(ctx, q, rng, nil, "followers belong to accounts, so this is workspace-wide"), res, reason), nil
}

func listPostAnalytics(ctx context.Context, d Deps, in ListPostAnalyticsInput) (*AnalyticsResult, error) {
	opt := repository.PostAnalyticsListOptions{Publisher: models.PublisherZernio, SortBy: in.SortBy, Order: in.Order,
		Page: max(in.Page, 1), Limit: 10}
	if opt.SortBy == "" {
		opt.SortBy = "engagement"
	}
	if !slices.Contains(postSorts, opt.SortBy) {
		return nil, assistantkit.Refused("sortBy is one of " + strings.Join(postSorts, ", "))
	}
	if opt.Order == "" {
		opt.Order = "desc"
	}
	if opt.Order != "asc" && opt.Order != "desc" {
		return nil, assistantkit.Refused("order is asc or desc")
	}
	if in.Limit > 0 {
		opt.Limit = min(in.Limit, 25)
	}
	a := Applied{Note: "every post published through Ogen, all time"}
	slug, err := insightPlatform(ctx, d, in.Platform)
	if err != nil {
		return nil, err
	}
	if slug != "" {
		a.Platforms = []string{slug}
		// The list filters by the platform's display name.
		if opt.Platform, err = d.Analytics.PlatformName(ctx, slug); err != nil {
			return nil, err
		}
	}
	res, reason, err := d.Analytics.ListPosts(ctx, opt)
	if err != nil {
		return nil, err
	}
	return result(a, res, reason), nil
}
