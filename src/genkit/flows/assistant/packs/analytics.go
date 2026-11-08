package packs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/analytics/performers"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

const analyticsPackName = "analytics"

// maxExamples caps the posts "more like these" learns from.
const maxExamples = 5

// analyticsPack explains how posts did, over the window on screen, and makes
// more of what worked from the ideas and assets behind it.
func analyticsPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    analyticsPackName,
		Summary: "say how posts performed over a window — totals, trends, best and worst posts — and write more posts like the ones that worked",
		Screens: []viewcontext.Screen{"analytics", "analytics.post", "campaign.analytics"},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			overview := genkit.DefineTool(g, "analyticsOverview",
				"Returns the analytics overview for a window: reach, interactions, posts published and followers, against the previous window, with trends and insights.",
				assistantkit.Wrap(func(ctx context.Context, in AnalyticsQuery) (any, error) { return analyticsOverview(ctx, d, in) }))
			top := genkit.DefineTool(g, "topPosts",
				"Returns a window's best and worst posts, scored against the workspace's typical post at the same age, with insights.",
				assistantkit.Wrap(func(ctx context.Context, in TopPostsInput) (any, error) { return topPosts(ctx, d, in) }))
			specs := []assistantkit.Spec{
				spec(analyticsPackName, overview, assistantkit.TierRead,
					`Call this when the user asks how things are going — "how did we do this month?", "is reach up?". The window defaults to the one on screen, else the last 28 days; the campaign to the thread's. Quote numbers as given; don't compute your own.`),
				spec(analyticsPackName, top, assistantkit.TierRead,
					`Call this for the best or worst posts — "what worked last month?", "which posts underperformed?". by is against_typical (default), reach, engagement_rate or interactions.`),
			}
			if d.Compose != nil && d.Links != nil {
				more := genkit.DefineTool(g, "moreLikeThese",
					"Writes new posts in the vein of posts that performed well — their angle and format, not their words — from the ideas and assets behind them, and saves them as drafts linked to the same ideas. More than a few are offered for the user to confirm first.",
					assistantkit.Gated("moreLikeThese", assistantkit.Gate[MoreLikeTheseInput]{
						Precheck: func(_ context.Context, in MoreLikeTheseInput) error { return checkExamples(in) },
						Needs:    func(_ context.Context, in MoreLikeTheseInput) bool { return overN(d, max(in.Count, 1)) },
						Summary: func(in MoreLikeTheseInput) string {
							return fmt.Sprintf("Write %s like %s", plural(max(in.Count, 1), "post", "posts"), plural(len(in.PostIDs), "top post", "top posts"))
						},
					}, assistantkit.Wrap(func(ctx context.Context, in MoreLikeTheseInput) (*DraftIdeasOutput, error) {
						return moreLikeThese(ctx, d, in)
					})))
				specs = append(specs, spec(analyticsPackName, more, assistantkit.TierProposal,
					`Call this when the user asks for more like the posts that worked — "make three more like the top performers". Pass postIds from topPosts (at most 5) and count. The new posts are drafts on the examples' platform and campaign unless you pass others.`))
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
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign; 'all' for the whole workspace"`
}

// rangeAndScope resolves a query against the screen: its window when the
// query names no dates, its platforms when the query names none, and the
// thread's campaign unless "all".
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
	case campaign == "":
		campaign = assistant.CampaignID(ctx)
	}
	sc, err := d.Analytics.Scope(ctx, platforms, campaign)
	switch {
	case errors.Is(err, analytics.ErrInvalidPlatform):
		return rng, nil, assistantkit.Refused("one of those platforms isn't known; use slugs like linkedin or instagram")
	case errors.Is(err, analytics.ErrCampaignNotFound):
		return rng, nil, assistantkit.Refused("that campaign doesn't exist")
	}
	return rng, sc, err
}

// Unavailable is a read with no data, and why.
type Unavailable struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

func analyticsOverview(ctx context.Context, d Deps, in AnalyticsQuery) (any, error) {
	rng, sc, err := rangeAndScope(ctx, d, in, "")
	if err != nil {
		return nil, err
	}
	resp, reason, err := d.Analytics.Overview(ctx, rng, sc)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return Unavailable{Reason: reason}, nil
	}
	return resp, nil
}

// TopPostsInput ranks a window's posts.
type TopPostsInput struct {
	AnalyticsQuery
	By    string `json:"by,omitempty"    jsonschema:"description=against_typical (default), reach, engagement_rate or interactions"`
	Limit int    `json:"limit,omitempty" jsonschema:"description=Posts per list; default 5"`
}

func topPosts(ctx context.Context, d Deps, in TopPostsInput) (any, error) {
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
	if res == nil {
		return Unavailable{Reason: reason}, nil
	}
	return res, nil
}

// MoreLikeTheseInput asks for posts like the examples.
type MoreLikeTheseInput struct {
	PostIDs     []string `json:"postIds"               jsonschema:"description=The posts that worked, at most 5"`
	Count       int      `json:"count,omitempty"       jsonschema:"description=How many new posts; default 1"`
	PlatformID  string   `json:"platformId,omitempty"  jsonschema:"description=Defaults to the first example's"`
	CampaignID  string   `json:"campaignId,omitempty"  jsonschema:"description=Defaults to the examples' campaign"`
	Instruction string   `json:"instruction,omitempty" jsonschema:"description=Steering for the writer"`
}

func checkExamples(in MoreLikeTheseInput) error {
	n := len(dedupe(in.PostIDs))
	if n == 0 || n > maxExamples {
		return assistantkit.Refused(fmt.Sprintf("pass 1 to %d example posts, e.g. from topPosts", maxExamples))
	}
	return nil
}

func moreLikeThese(ctx context.Context, d Deps, in MoreLikeTheseInput) (*DraftIdeasOutput, error) {
	if err := checkExamples(in); err != nil {
		return nil, err
	}
	count := min(max(in.Count, 1), max(d.DraftMax, 1))
	var examples []models.Post
	var ideaIDs, assetIDs []string
	var ideaLines []string
	for _, id := range dedupe(in.PostIDs) {
		p, err := d.Posts.GetByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, assistantkit.Refused(fmt.Sprintf("there is no post %q", id))
		}
		if err != nil {
			return nil, err
		}
		examples = append(examples, *p)
		assetIDs = appendNew(assetIDs, p.UsedAssetIDs...)
		ideas, err := d.Links.IdeasOfPost(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, i := range ideas {
			if !slices.Contains(ideaIDs, i.ID) {
				ideaIDs = append(ideaIDs, i.ID)
				ideaLines = append(ideaLines, i.Title)
			}
		}
	}
	campaign := in.CampaignID
	if campaign == "" {
		campaign = examples[0].CampaignID
	}
	platform := in.PlatformID
	if platform == "" {
		platform = examples[0].PlatformID
	}
	if platform == "" {
		return nil, assistantkit.Refused("the examples have no platform; pass platformId")
	}

	var b strings.Builder
	b.WriteString("These posts performed well. Write new posts in the same vein — the same kind of angle, hook and format — on new points; never repeat or paraphrase them.\n")
	for i, p := range examples {
		fmt.Fprintf(&b, "\nExample %d: %s\n%s\n", i+1, p.Title, excerpt(p.Content, 1200))
	}
	if len(ideaLines) > 0 {
		b.WriteString("\nThe ideas behind them: " + strings.Join(ideaLines, "; ") + "\n")
	}
	res, err := d.Compose.Write(ctx, compose.DraftRequest{
		CampaignID: campaign, PlatformID: platform, Count: count, SourceMaterial: b.String(),
		Instruction: in.Instruction, UsedAssetIDs: assetIDs, CreatedBy: actor(ctx),
	}, ideaIDs, func(p compose.DraftedPost) {
		assistantkit.Touch(ctx, "post", p.PostID)
		assistant.Emit(ctx, "idea_draft_post", p)
	})
	if err != nil && (res == nil || len(res.Posts) == 0) {
		return nil, composeErr(err)
	}
	out := &DraftIdeasOutput{Posts: res.Posts, Warnings: res.Warnings}
	if err != nil {
		out.Warnings = append(out.Warnings, "stopped early: "+err.Error())
	}
	return out, nil
}

func excerpt(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func appendNew(dst []string, ids ...string) []string {
	for _, id := range ids {
		if !slices.Contains(dst, id) {
			dst = append(dst, id)
		}
	}
	return dst
}
