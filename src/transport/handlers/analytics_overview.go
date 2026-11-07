package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// Overview serves the CON-237 "what happened over last N days" dashboard: five
// KPI cards, per-metric current/previous series, and deterministic insights, in
// one tenant-scoped call. Latest-per-post values come from post_analytics_current;
// "posts published" is composed from the main-DB posts table. The
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
// @Param        platform     query []string false "Zernio platform slug; repeat or comma-separate values for a union (default every platform)" collectionFormat(multi)
// @Param        campaign_id  query string false "Narrow to one campaign (default the whole workspace); the followers card stays workspace-wide (followers_scope)"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Router       /api/analytics/overview [get]
func (h *AnalyticsHandler) Overview(c *fiber.Ctx) error {
	if h.repo == nil {
		// Analytics DB disabled for this deployment — degrade gracefully.
		return c.JSON(insightEnvelope{Available: false, Reason: analytics.ReasonNotConfigured})
	}
	rng, err := timeframe.Resolve(c.Query("from"), c.Query("to"), c.Query("window"), c.Query("granularity"), time.Now().UTC())
	if err != nil {
		if errors.Is(err, timeframe.ErrWindowTooLarge) {
			return fiber.NewError(fiber.StatusBadRequest, "window_too_large")
		}
		return fiber.NewError(fiber.StatusBadRequest, "invalid_range")
	}
	scope, err := h.parseScope(c)
	if err != nil {
		return err
	}
	resp, reason, err := h.Service().Overview(reqCtx(c), rng, scope)
	if err != nil {
		return err
	}
	if resp == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reason})
	}
	return c.JSON(insightEnvelope{Available: true, Data: *resp})
}
