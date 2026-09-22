package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Availability reasons on the graceful insight envelope (CON-153).
const (
	reasonAddonRequired = "addon_required"
	reasonNotConfigured = "not_configured"
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

var validFollowerGranularity = map[string]bool{"daily": true, "weekly": true, "monthly": true}

// serveInsight runs the shared live-proxy flow for the three aggregate
// endpoints: resolve the tenant profile, validate the shared query params,
// call fetch, and wrap the result in the graceful envelope. fetch is the
// per-endpoint Zernio call; it returns the JSON `data` payload.
func (h *AnalyticsHandler) serveInsight(c *fiber.Ctx, fetch func(ctx context.Context, q zernio.InsightQuery) (any, error)) error {
	if h.client == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reasonNotConfigured})
	}
	profileID, err := h.resolveProfile(reqCtx(c))
	if err != nil {
		return err
	}
	if profileID == "" {
		return c.JSON(insightEnvelope{Available: false, Reason: reasonNotConfigured})
	}
	source := c.Query("source")
	if source != "" && !validInsightSource[source] {
		return fiber.NewError(fiber.StatusBadRequest, "source must be all, late, or external")
	}
	data, err := fetch(reqCtx(c), zernio.InsightQuery{
		ProfileID: profileID,
		Platform:  c.Query("platform"),
		AccountID: c.Query("account_id"),
		Source:    source,
	})
	if err != nil {
		if errors.Is(err, zernio.ErrAnalyticsUnavailable) {
			return c.JSON(insightEnvelope{Available: false, Reason: reasonAddonRequired})
		}
		return fiber.NewError(fiber.StatusBadGateway, "analytics upstream unavailable")
	}
	return c.JSON(insightEnvelope{Available: true, Data: data})
}

// resolveProfile returns the current tenant's Zernio profile id, or "" when
// the integration isn't configured for this tenant.
func (h *AnalyticsHandler) resolveProfile(ctx context.Context) (string, error) {
	if h.profileID == nil {
		return "", nil
	}
	return h.profileID(ctx)
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
	return h.serveInsight(c, func(ctx context.Context, q zernio.InsightQuery) (any, error) {
		out, err := h.client.GetBestTimes(ctx, q)
		if err != nil {
			return nil, err
		}
		return fiber.Map{"slots": out.Slots}, nil
	})
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
	return h.serveInsight(c, func(ctx context.Context, q zernio.InsightQuery) (any, error) {
		out, err := h.client.GetContentDecay(ctx, q)
		if err != nil {
			return nil, err
		}
		return fiber.Map{"buckets": out.Buckets}, nil
	})
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
	return h.serveInsight(c, func(ctx context.Context, q zernio.InsightQuery) (any, error) {
		out, err := h.client.GetPostingFrequency(ctx, q)
		if err != nil {
			return nil, err
		}
		return fiber.Map{"frequency": out.Frequency}, nil
	})
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
	accountID := c.Query("account_id")
	granularity := c.Query("granularity", "daily")
	if !validFollowerGranularity[granularity] {
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

	summary, err := h.followerRepo.Summary(reqCtx(c), accountID)
	if err != nil {
		return err
	}
	points, err := h.followerRepo.Series(reqCtx(c), repository.FollowerSeriesOptions{
		AccountID: accountID,
		From:      from,
		To:        to,
	})
	if err != nil {
		return err
	}
	points = downsampleSeries(points, granularity)

	series := map[string][]repository.FollowerSeriesPoint{}
	for _, p := range points {
		series[p.SocialAccountID] = append(series[p.SocialAccountID], p)
	}
	if summary == nil {
		summary = []repository.FollowerAccountSummary{}
	}
	return c.JSON(insightEnvelope{Available: true, Data: fiber.Map{
		"accounts":    summary,
		"series":      series,
		"granularity": granularity,
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

// downsampleSeries reduces daily points to one point per ISO-week or calendar
// month per account, keeping the last (latest) point in each bucket. The input
// is ordered by (account, date ASC), so a later point in the same bucket wins
// and the first-seen bucket order stays ascending. daily returns the input
// unchanged.
func downsampleSeries(points []repository.FollowerSeriesPoint, granularity string) []repository.FollowerSeriesPoint {
	if granularity != "weekly" && granularity != "monthly" {
		return points
	}
	out := make([]repository.FollowerSeriesPoint, 0, len(points))
	idxByBucket := map[string]int{}
	for _, p := range points {
		key := p.SocialAccountID + "|" + periodBucket(p.PointDate, granularity)
		if i, ok := idxByBucket[key]; ok {
			out[i] = p
			continue
		}
		idxByBucket[key] = len(out)
		out = append(out, p)
	}
	return out
}

func periodBucket(d time.Time, granularity string) string {
	if granularity == "weekly" {
		y, w := d.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	}
	return fmt.Sprintf("%04d-%02d", d.Year(), int(d.Month()))
}
