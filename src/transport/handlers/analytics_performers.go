package handlers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/performers"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Performers serves the CON-238 "performers and outliers" board: the window's
// posts ranked and scored against the account's typical post for its platform
// and age, split into Best/Worst lists, with deterministic insights. Candidates
// + display fields come from post_analytics_current; the per-platform
// expected-at-age baseline is computed from the snapshot history (join to
// current for published_at). The account block carries username/id from the
// current row's platform breakdown, labelled with display_name/avatar_url from
// social_accounts (best-effort, username fallback).
//
// Performers godoc
// @Summary      Performers and outliers
// @Description  Best/Worst posts for a window, age-adjusted vs the account's typical, plus insights.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        window   query string false "Relative window shorthand, e.g. 28d (default 28d)"
// @Param        from     query string false "Inclusive start date YYYY-MM-DD (with to, overrides window)"
// @Param        to       query string false "Inclusive end date YYYY-MM-DD"
// @Param        by       query string false "against_typical|reach|engagement_rate|interactions (default against_typical)"
// @Param        limit    query int    false "Rows per list, default 5, clamped"
// @Param        platform query []string false "Zernio platform slug; repeat or comma-separate values for a union (default every platform)" collectionFormat(multi)
// @Param        campaign_id query string false "Narrow the ranked posts to one campaign (default the whole workspace); typical stays workspace-wide"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Router       /api/analytics/performers [get]
func (h *AnalyticsHandler) Performers(c *fiber.Ctx) error {
	if h.repo == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: "not_configured"})
	}

	rng, err := timeframe.Resolve(c.Query("from"), c.Query("to"), c.Query("window"), "", time.Now().UTC())
	if err != nil {
		if errors.Is(err, timeframe.ErrWindowTooLarge) {
			return fiber.NewError(fiber.StatusBadRequest, "window_too_large")
		}
		return fiber.NewError(fiber.StatusBadRequest, "invalid_range")
	}
	by := c.Query("by", performers.ByAgainstTypical)
	if !performers.ValidBy(by) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_sort")
	}
	limit := performers.DefaultLimit
	if v := c.Query("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			limit = n
		}
	}
	scope, err := h.parseScope(c)
	if err != nil {
		return err
	}

	ctx := reqCtx(c)
	cur, err := h.repo.PublishedBetween(ctx, rng.From, rng.To)
	if err != nil {
		return err
	}
	if err := h.filterRows(ctx, scope, &cur); err != nil {
		return err
	}
	if len(cur) == 0 {
		return c.JSON(insightEnvelope{Available: false, Reason: "no_data"})
	}

	// The typical-at-age baseline stays workspace-wide under any scope, so a
	// campaign's posts are scored against the workspace's usual post rather than
	// against each other.
	samples, err := h.repo.ReachByAgeSamples(ctx)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(cur))
	seen := map[string]bool{}
	for i := range cur {
		if id, _ := accountRef(&cur[i]); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	known := h.loadAccounts(ctx, ids)

	now := time.Now().UTC()
	cands := make([]performers.Candidate, 0, len(cur))
	for i := range cur {
		r := cur[i]
		if r.PublishedAt == nil {
			continue
		}
		cands = append(cands, performers.Candidate{
			PostID:          r.PostID,
			PublisherPostID: r.PublisherPostID,
			Title:           r.Title,
			Platform:        r.Platform,
			Account:         performersAccount(&r, known),
			Reach:           r.Reach,
			Impressions:     r.Impressions,
			Likes:           r.Likes,
			Comments:        r.Comments,
			Shares:          r.Shares,
			EngagementRate:  r.EngagementRate,
			PublishedAt:     *r.PublishedAt,
			AgeDays:         daysBetween(*r.PublishedAt, now),
		})
	}

	res := performers.Build(cands, toPerfSamples(samples), performers.Options{By: by, Limit: limit})

	return c.JSON(insightEnvelope{Available: true, Data: fiber.Map{
		"window":      fiber.Map{"from": rng.FromISO(), "to": rng.ToISO(), "days": rng.Days},
		"updated_at":  maxLastChecked(cur),
		"by":          res.By,
		"total_posts": res.TotalPosts,
		"best":        res.Best,
		"worst":       res.Worst,
		"insights":    res.Insights,
	}})
}

func performersAccount(r *models.PostAnalytics, known map[string]models.SocialAccount) performers.Account {
	id, username := accountRef(r)
	l := labelAccount(id, username, known)
	return performers.Account{ID: l.ID, Username: l.Username, DisplayName: l.DisplayName, AvatarURL: l.AvatarURL}
}

func toPerfSamples(in []repository.ReachAgeSample) []performers.Sample {
	out := make([]performers.Sample, len(in))
	for i, s := range in {
		out[i] = performers.Sample{
			Platform:     s.Platform,
			PostID:       s.PostID,
			AgeDay:       s.AgeDay,
			Reach:        s.Reach,
			Interactions: s.Interactions,
		}
	}
	return out
}

func daysBetween(from, to time.Time) int {
	d := int(to.Sub(from) / (24 * time.Hour))
	if d < 0 {
		return 0
	}
	return d
}
