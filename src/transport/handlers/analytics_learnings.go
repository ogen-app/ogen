package handlers

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/learnings"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// Learnings serves the CON-239 "What we've learned" board: an all-time day×hour
// heatmap, a post-lifespan accrual curve, and deterministic structural
// works/fading patterns. It is decoupled from the shared dashboard window;
// callers may pass an optional `since` baseline and a `trend_window` (drives the
// fading comparison). Latest metrics + display come from post_analytics_current;
// structural attributes from the main-DB posts table (joined app-side by id);
// the lifespan curve from the snapshot history at hour resolution.
//
// Learnings godoc
// @Summary      Learnings — all-time posting knowledge
// @Description  Heatmap + post-lifespan curve + structural what-works/fading patterns.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        since         query string false "Optional baseline lower bound YYYY-MM-DD (default all-time)"
// @Param        trend_window  query string false "Fading comparison window, e.g. 90d/3mo/12w (default 90d)"
// @Param        metric        query string false "reach|saves (default reach)"
// @Param        platform      query []string false "Zernio platform slug; repeat or comma-separate values for a union (default every platform)" collectionFormat(multi)
// @Param        campaign_id   query string false "Not supported: any value is rejected with 400 campaign_scope_unsupported (lessons are workspace-wide)"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
// @Router       /api/analytics/learnings [get]
func (h *AnalyticsHandler) Learnings(c *fiber.Ctx) error {
	if h.repo == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: analytics.ReasonNotConfigured})
	}
	since, ok := parseSince(c.Query("since"))
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_param")
	}
	trendDays, ok := analytics.ParseTrendWindow(c.Query("trend_window"))
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_param")
	}
	metric := c.Query("metric", learnings.MetricReach)
	if metric != learnings.MetricReach && metric != learnings.MetricSaves {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_param")
	}
	// An all-time lesson mined from one campaign is a much thinner claim than a
	// workspace lesson, so the campaign filter is refused rather than honoured.
	if strings.TrimSpace(c.Query("campaign_id")) != "" {
		return fiber.NewError(fiber.StatusBadRequest, "campaign_scope_unsupported")
	}
	scope, err := h.parsePlatformScope(c)
	if err != nil {
		return err
	}
	resp, reason, err := h.service().Lessons(reqCtx(c), scope, analytics.LessonsOptions{
		Since: since, TrendWindowDays: trendDays, Metric: metric,
	})
	if err != nil {
		return err
	}
	if resp == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reason})
	}
	return c.JSON(insightEnvelope{Available: true, Data: *resp})
}

// parseSince returns (nil, true) for empty, (&date, true) for a valid date, or
// (nil, false) for a malformed value.
func parseSince(s string) (*time.Time, bool) {
	if s == "" {
		return nil, true
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, false
	}
	return &t, true
}
