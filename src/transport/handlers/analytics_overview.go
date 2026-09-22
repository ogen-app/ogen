package handlers

import (
	"errors"
	"slices"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/overview"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Overview serves the CON-237 "what happened over last N days" dashboard: five
// KPI cards, per-metric current/previous series, and deterministic insights, in
// one tenant-scoped call. Latest-per-post values come from post_analytics_current
// (CON-236); "posts published" is composed from the main-DB posts table. The
// "usual range" baseline band is not yet computed (no tenant has enough history)
// — cards report baseline "insufficient_history".
//
// Overview godoc
// @Summary      Cumulative analytics overview
// @Description  KPI cards + per-metric series + deterministic insights for a window.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        window       query string false "Relative window shorthand, e.g. 28d/12w/6mo (default 28d)"
// @Param        from         query string false "Inclusive start date YYYY-MM-DD (with to, overrides window)"
// @Param        to           query string false "Inclusive end date YYYY-MM-DD"
// @Param        granularity  query string false "day|week|month (default adaptive)"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
// @Router       /api/analytics/overview [get]
func (h *AnalyticsHandler) Overview(c *fiber.Ctx) error {
	if h.repo == nil {
		// Analytics DB disabled for this deployment — degrade gracefully.
		return c.JSON(insightEnvelope{Available: false, Reason: "not_configured"})
	}

	rng, err := timeframe.Resolve(c.Query("from"), c.Query("to"), c.Query("window"), c.Query("granularity"), time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, timeframe.ErrWindowTooLarge):
			return fiber.NewError(fiber.StatusBadRequest, "window_too_large")
		default:
			return fiber.NewError(fiber.StatusBadRequest, "invalid_range")
		}
	}
	prev := rng.Previous()
	ctx := reqCtx(c)

	curPosts, err := h.repo.PublishedBetween(ctx, rng.From, rng.To)
	if err != nil {
		return err
	}
	prevPosts, err := h.repo.PublishedBetween(ctx, prev.From, prev.To)
	if err != nil {
		return err
	}

	var curPub, prevPub []time.Time
	if h.posts != nil {
		if curPub, err = h.posts.PublishedAtsBetween(ctx, rng.From, rng.To); err != nil {
			return err
		}
		if prevPub, err = h.posts.PublishedAtsBetween(ctx, prev.From, prev.To); err != nil {
			return err
		}
	}

	var folTotals []overview.FollowerDayTotal
	var folNow int
	if h.followerRepo != nil {
		summary, err := h.followerRepo.Summary(ctx, "")
		if err != nil {
			return err
		}
		for _, a := range summary {
			folNow += int(a.CurrentFollowers)
		}
		// One series spanning both windows lets Build derive current and previous
		// follower levels without a second fetch.
		pts, err := h.followerRepo.Series(ctx, repository.FollowerSeriesOptions{From: prev.From, To: rng.To})
		if err != nil {
			return err
		}
		folTotals = followerDailyTotals(pts)
	}

	// Truly empty tenant (no analytics, no posts, no followers) → graceful no_data.
	if len(curPosts) == 0 && len(prevPosts) == 0 && len(curPub) == 0 && len(prevPub) == 0 && folNow == 0 && len(folTotals) == 0 {
		return c.JSON(insightEnvelope{Available: false, Reason: "no_data"})
	}

	resp := overview.Build(overview.Inputs{
		Rng:            rng,
		Prev:           prev,
		CurPosts:       toPostPoints(curPosts),
		PrevPosts:      toPostPoints(prevPosts),
		CurPublished:   curPub,
		PrevPublished:  prevPub,
		FollowerTotals: folTotals,
		FollowersNow:   folNow,
		UpdatedAt:      maxLastChecked(curPosts, prevPosts),
	})
	return c.JSON(insightEnvelope{Available: true, Data: resp})
}

// toPostPoints maps current-state rows to the assembly input, folding the
// interactions definition (likes+comments+shares) once here.
func toPostPoints(rows []models.PostAnalytics) []overview.PostPoint {
	out := make([]overview.PostPoint, 0, len(rows))
	for i := range rows {
		r := rows[i]
		if r.PublishedAt == nil {
			continue
		}
		out = append(out, overview.PostPoint{
			At:           *r.PublishedAt,
			Reach:        r.Reach,
			Interactions: r.Likes + r.Comments + r.Shares,
		})
	}
	return out
}

// followerDailyTotals sums the per-account follower points into one total per
// calendar day, ascending. Zernio refreshes followers daily, so one point per
// account per day summed by date is the whole-workspace level.
func followerDailyTotals(pts []repository.FollowerSeriesPoint) []overview.FollowerDayTotal {
	byDay := map[time.Time]int{}
	for _, p := range pts {
		d := truncDay(p.PointDate)
		byDay[d] += int(p.Followers)
	}
	out := make([]overview.FollowerDayTotal, 0, len(byDay))
	for d, t := range byDay {
		out = append(out, overview.FollowerDayTotal{Date: d, Total: t})
	}
	slices.SortFunc(out, func(a, b overview.FollowerDayTotal) int { return a.Date.Compare(b.Date) })
	return out
}

func maxLastChecked(sets ...[]models.PostAnalytics) time.Time {
	var max time.Time
	for _, s := range sets {
		for i := range s {
			if s[i].LastCheckedAt.After(max) {
				max = s[i].LastCheckedAt
			}
		}
	}
	return max
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
