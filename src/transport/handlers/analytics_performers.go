package handlers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/performers"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/usecase/analytics"
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
		return c.JSON(insightEnvelope{Available: false, Reason: analytics.ReasonNotConfigured})
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
	res, reason, err := h.Service().Performers(reqCtx(c), rng, scope, by, limit)
	if err != nil {
		return err
	}
	if res == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reason})
	}
	return c.JSON(insightEnvelope{Available: true, Data: res})
}
