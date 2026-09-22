package handlers

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/learnings"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
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
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
// @Router       /api/analytics/learnings [get]
func (h *AnalyticsHandler) Learnings(c *fiber.Ctx) error {
	if h.repo == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: "not_configured"})
	}

	since, ok := parseSince(c.Query("since"))
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_param")
	}
	trendDays, ok := parseTrendDays(c.Query("trend_window"))
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_param")
	}
	metric := c.Query("metric", learnings.MetricReach)
	if metric != learnings.MetricReach && metric != learnings.MetricSaves {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_param")
	}

	ctx := reqCtx(c)
	current, err := h.repo.CurrentByPostID(ctx)
	if err != nil {
		return err
	}
	if len(current) == 0 {
		return c.JSON(insightEnvelope{Available: false, Reason: "no_data"})
	}

	var sinceVal time.Time
	if since != nil {
		sinceVal = *since
	}
	var posts []models.Post
	if h.posts != nil {
		if posts, err = h.posts.ListPublishedSince(ctx, sinceVal); err != nil {
			return err
		}
	}
	lifeSamples, err := h.repo.LifespanSamples(ctx, sinceVal)
	if err != nil {
		return err
	}

	facts := make([]learnings.PostFact, 0, len(posts))
	for i := range posts {
		p := posts[i]
		a := current[p.ID]
		if a == nil || a.PublishedAt == nil {
			continue
		}
		if since != nil && a.PublishedAt.Before(*since) {
			continue
		}
		facts = append(facts, learnings.PostFact{
			PublishedAt:   *a.PublishedAt,
			Platform:      a.Platform,
			Reach:         a.Reach,
			Saves:         a.Saves,
			MediaCount:    len(p.MediaURLs),
			IsVideo:       isVideoPost(p.PlatformPostType),
			ContentLength: utf8.RuneCountInString(p.Content),
			HashtagCount:  strings.Count(p.Content, "#"),
			HasLink:       hasLink(p),
		})
	}

	resp := learnings.Build(learnings.Inputs{
		Now:             time.Now().UTC(),
		Since:           since,
		TrendWindowDays: trendDays,
		Metric:          metric,
		Posts:           facts,
		Lifespan:        toLifespanPoints(lifeSamples),
		UpdatedAt:       maxLastCheckedMap(current),
	})
	return c.JSON(insightEnvelope{Available: true, Data: resp})
}

var trendWindowRe = regexp.MustCompile(`^(\d+)(d|w|mo)$`)

// parseSince returns (nil, true) for empty, (&date, true) for a valid date, or
// (nil, false) for a malformed value.
func parseSince(s string) (*time.Time, bool) {
	if s == "" {
		return nil, true
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// parseTrendDays resolves the fading comparison window to whole days; empty
// defaults to 90.
func parseTrendDays(s string) (int, bool) {
	if s == "" {
		return 90, true
	}
	m := trendWindowRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch m[2] {
	case "d":
		return n, true
	case "w":
		return n * 7, true
	case "mo":
		return n * 30, true
	}
	return 0, false
}

func isVideoPost(platformPostType string) bool {
	t := strings.ToLower(platformPostType)
	return strings.Contains(t, "video") || strings.Contains(t, "reel") || strings.Contains(t, "short")
}

func hasLink(p models.Post) bool {
	return p.CTAUrl != "" || strings.Contains(strings.ToLower(p.Content), "http")
}

func toLifespanPoints(in []repository.LifespanSample) []learnings.LifespanPoint {
	out := make([]learnings.LifespanPoint, len(in))
	for i, s := range in {
		out[i] = learnings.LifespanPoint{PostID: s.PostID, AgeHours: s.AgeHours, Reach: s.Reach}
	}
	return out
}

func maxLastCheckedMap(current map[string]*models.PostAnalytics) time.Time {
	var max time.Time
	for _, a := range current {
		if a.LastCheckedAt.After(max) {
			max = a.LastCheckedAt
		}
	}
	return max
}
