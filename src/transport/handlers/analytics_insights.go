package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// insightEnvelope is the graceful response wrapper shared by the CON-153
// analytics endpoints. When Available is false the caller lacks the data
// source (Analytics add-on missing, or Zernio not configured for the
// tenant) and Data is null — a 200, so the UI renders an upsell/empty state
// without error handling.
type insightEnvelope struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Data      any    `json:"data"`
}

var validInsightSource = map[string]bool{"all": true, "late": true, "external": true}

const (
	// insightCacheTTL is how long a live insight read is reused. The insights
	// aggregate weeks of engagement, so a few minutes of staleness is invisible,
	// while one analytics page view otherwise makes three upstream calls.
	insightCacheTTL     = 10 * time.Minute
	insightCacheEntries = 2048
)

// insightFilter reads the shared query params of the three live insight
// endpoints.
func insightFilter(c *fiber.Ctx) (analytics.InsightFilter, error) {
	source := c.Query("source")
	if source != "" && !validInsightSource[source] {
		return analytics.InsightFilter{}, fiber.NewError(fiber.StatusBadRequest, "source must be all, late, or external")
	}
	return analytics.InsightFilter{Platform: c.Query("platform"), AccountID: c.Query("account_id"), Source: source}, nil
}

// serveInsight wraps one live insight read in the graceful envelope: no
// data source is a 200 with available false, a publisher failure a 502.
// The reads are cached per endpoint, profile and filter for
// insightCacheTTL, and concurrent requests for one share a single call.
func serveInsight[T any](c *fiber.Ctx, read func(ctx context.Context, f analytics.InsightFilter) (T, string, error), wrap func(T) fiber.Map) error {
	f, err := insightFilter(c)
	if err != nil {
		return err
	}
	data, reason, err := read(reqCtx(c), f)
	if errors.Is(err, analytics.ErrUpstream) {
		return fiber.NewError(fiber.StatusBadGateway, "analytics upstream unavailable")
	}
	if err != nil {
		return err
	}
	if reason != "" {
		return c.JSON(insightEnvelope{Available: false, Reason: reason})
	}
	return c.JSON(insightEnvelope{Available: true, Data: wrap(data)})
}

// BestTimes godoc
// @Summary      Best times to post
// @Description  Day-of-week × hour engagement grid for the tenant's connected accounts (CON-153). Live read-through to Zernio, scoped to the tenant's profile. Returns {available:false, reason:"addon_required"} when the tenant lacks the Analytics add-on.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        platform    query  string  false  "Platform filter (e.g. instagram)"
// @Param        account_id  query  string  false  "Social account id filter"
// @Param        source      query  string  false  "all|late|external (default all)"
// @Success      200  {object}  handlers.insightEnvelope
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/analytics/best-times [get]
func (h *AnalyticsHandler) BestTimes(c *fiber.Ctx) error {
	return serveInsight(c, h.Service().BestTimes, func(slots []zernio.BestTimeSlot) fiber.Map { return fiber.Map{"slots": slots} })
}

// ContentDecay godoc
// @Summary      Content performance decay
// @Description  Engagement-accrual buckets showing what share of a post's total engagement is reached by each time window (CON-153). Live read-through to Zernio, scoped to the tenant's profile.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        platform    query  string  false  "Platform filter"
// @Param        account_id  query  string  false  "Social account id filter"
// @Param        source      query  string  false  "all|late|external (default all)"
// @Success      200  {object}  handlers.insightEnvelope
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/analytics/content-decay [get]
func (h *AnalyticsHandler) ContentDecay(c *fiber.Ctx) error {
	return serveInsight(c, h.Service().ContentDecay, func(b []zernio.ContentDecayBucket) fiber.Map { return fiber.Map{"buckets": b} })
}

// PostingFrequency godoc
// @Summary      Posting frequency vs engagement
// @Description  Per-platform correlation between posts-per-week and engagement rate (CON-153). Live read-through to Zernio, scoped to the tenant's profile.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        platform    query  string  false  "Platform filter"
// @Param        account_id  query  string  false  "Social account id filter"
// @Param        source      query  string  false  "all|late|external (default all)"
// @Success      200  {object}  handlers.insightEnvelope
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/analytics/posting-frequency [get]
func (h *AnalyticsHandler) PostingFrequency(c *fiber.Ctx) error {
	return serveInsight(c, h.Service().PostingFrequency, func(f []zernio.PostingFrequencyRow) fiber.Map { return fiber.Map{"frequency": f} })
}

// Followers godoc
// @Summary      Follower stats
// @Description  Follower-count history and growth per connected account (CON-153). Served entirely from the local daily snapshots (no publisher call on the request path); the daily refresh job keeps them current.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        account_id   query  string  false  "Social account id filter"
// @Param        from         query  string  false  "Start date YYYY-MM-DD"
// @Param        to           query  string  false  "End date YYYY-MM-DD"
// @Param        granularity  query  string  false  "daily|weekly|monthly (default daily)"
// @Success      200  {object}  handlers.insightEnvelope
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/analytics/followers [get]
func (h *AnalyticsHandler) Followers(c *fiber.Ctx) error {
	if h.followerRepo == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "follower analytics is not available")
	}
	granularity := c.Query("granularity", analytics.GranularityDaily)
	if !analytics.ValidGranularity(granularity) {
		return fiber.NewError(fiber.StatusBadRequest, "granularity must be daily, weekly, or monthly")
	}
	from, err := parseDateQuery(c.Query("from"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "from must be a YYYY-MM-DD date")
	}
	to, err := parseDateQuery(c.Query("to"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "to must be a YYYY-MM-DD date")
	}
	history, _, err := h.Service().FollowerHistory(reqCtx(c), analytics.FollowerHistoryOptions{
		AccountID: c.Query("account_id"), From: from, To: to, Granularity: granularity,
	})
	if err != nil {
		return err
	}
	return c.JSON(insightEnvelope{Available: true, Data: fiber.Map{
		"accounts":    history.Accounts,
		"series":      history.Series,
		"granularity": history.Granularity,
	}})
}

// parseDateQuery parses a YYYY-MM-DD query param; empty is the zero time
// (unbounded). Any other malformed value is an error.
func parseDateQuery(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}
