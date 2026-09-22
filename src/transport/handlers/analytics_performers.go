package handlers

import (
	"errors"
	"strconv"
	"strings"
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
// current row's platform breakdown; display_name/avatar enrichment from
// social_accounts is a follow-up (best-effort, username fallback per the PRD).
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
// @Param        platform query string false "Optional platform filter"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
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
	platform := c.Query("platform")

	ctx := reqCtx(c)
	cur, err := h.repo.PublishedBetween(ctx, rng.From, rng.To)
	if err != nil {
		return err
	}
	if platform != "" {
		cur = filterByPlatform(cur, platform)
	}
	if len(cur) == 0 {
		return c.JSON(insightEnvelope{Available: false, Reason: "no_data"})
	}

	samples, err := h.repo.ReachByAgeSamples(ctx)
	if err != nil {
		return err
	}

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
			Account:         accountFor(r),
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

func filterByPlatform(rows []models.PostAnalytics, platform string) []models.PostAnalytics {
	out := rows[:0:0]
	for _, r := range rows {
		if strings.EqualFold(r.Platform, platform) {
			out = append(out, r)
		}
	}
	return out
}

// accountFor pulls the owning account's username/id from the current row's
// per-platform breakdown, preferring the entry that matches the row's platform.
func accountFor(r models.PostAnalytics) performers.Account {
	var username, id string
	for _, pa := range r.PlatformAnalytics {
		if strings.EqualFold(pa.Platform, r.Platform) {
			username, id = pa.AccountUsername, pa.AccountID
			break
		}
	}
	if username == "" && len(r.PlatformAnalytics) > 0 {
		username, id = r.PlatformAnalytics[0].AccountUsername, r.PlatformAnalytics[0].AccountID
	}
	return performers.Account{ID: id, Username: username, DisplayName: username}
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
