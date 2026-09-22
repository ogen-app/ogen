package handlers

import (
	"context"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// postAnalyticsResponse is the per-post analytics snapshot shape
// (CON-93 §7, GET /api/posts/:id/analytics). The aggregate metrics are
// nested under `analytics`; the per-platform breakdown carries each
// platform's sync_status / error_message so a caller can prompt a
// reconnect for platforms missing analytics scope while still reading
// metrics for the platforms that do report.
type postAnalyticsResponse struct {
	PostID             string                       `json:"post_id"`
	Publisher          string                       `json:"publisher"`
	PublisherPostID    string                       `json:"publisher_post_id"`
	SyncStatus         string                       `json:"sync_status"`
	MetricsLastUpdated *time.Time                   `json:"metrics_last_updated"`
	LastRefreshedAt    time.Time                    `json:"last_refreshed_at"`
	Analytics          models.PostAnalyticsMetrics  `json:"analytics"`
	PlatformAnalytics  models.PlatformAnalyticsList `json:"platform_analytics"`

	// Status is "pending" ONLY on the no-snapshot-yet variant, where the
	// 200 body is just {status, post_id} (the background refresh hasn't
	// covered this post). Omitted on a real snapshot — don't confuse it
	// with SyncStatus, which is the publisher sync state of an existing
	// snapshot. Declared here so the single Swagger 2.0 success schema
	// documents both 200 shapes (Swagger 2.0 allows one schema per code).
	Status string `json:"status,omitempty"`
}

func newPostAnalyticsResponse(post *models.Post, a *models.PostAnalytics) postAnalyticsResponse {
	platforms := a.PlatformAnalytics
	if platforms == nil {
		platforms = models.PlatformAnalyticsList{}
	}
	return postAnalyticsResponse{
		PostID:             a.PostID,
		Publisher:          post.Publisher,
		PublisherPostID:    a.PublisherPostID,
		SyncStatus:         a.SyncStatus,
		MetricsLastUpdated: a.MetricsLastUpdated,
		// LastCheckedAt is when the refresh last looked; exposed as
		// last_refreshed_at for API compatibility. It is bumped every check, so
		// this stays accurate even when dedup writes no new history row (CON-236).
		LastRefreshedAt:   a.LastCheckedAt,
		Analytics:         a.Metrics(),
		PlatformAnalytics: platforms,
	}
}

// ProfileIDResolver returns the current tenant's Zernio profile id, read
// from the tenant-scoped settings on each call, or "" when the tenant has
// no profile (integration not configured for them).
type ProfileIDResolver func(ctx context.Context) (string, error)

// AnalyticsHandler serves the analytics surface under /api/analytics
// (CON-93 FR5 + CON-153). The post overview (/posts) and follower series
// (/followers) are served entirely from the database; the three insight
// aggregates (best-times/content-decay/posting-frequency) are live
// read-through proxies to Zernio, scoped to the tenant's profile. It lives
// under a separate /api/analytics group so its routes don't collide with
// /api/posts/:id.
type AnalyticsHandler struct {
	repo         repository.PostAnalyticsRepository
	followerRepo repository.FollowerStatsRepository
	posts        repository.PostRepository // main DB; CON-237 "posts published" count/series
	client       *zernio.Client
	profileID    ProfileIDResolver
	auth         fiber.Handler
}

// NewAnalyticsHandler wires the handler. followerRepo/client/profileID are
// CON-153 additions; a nil client (or nil resolver) makes the live-proxy
// insight endpoints report {available:false, reason:"not_configured"}, and a
// nil followerRepo makes /followers return 503 — matching the existing
// analytics-disabled behaviour.
func NewAnalyticsHandler(repo repository.PostAnalyticsRepository, followerRepo repository.FollowerStatsRepository, posts repository.PostRepository, client *zernio.Client, profileID ProfileIDResolver, auth fiber.Handler) *AnalyticsHandler {
	return &AnalyticsHandler{repo: repo, followerRepo: followerRepo, posts: posts, client: client, profileID: profileID, auth: auth}
}

func (h *AnalyticsHandler) Register(app *fiber.App) {
	g := app.Group("/api/analytics")
	g.Get("/overview", h.auth, h.Overview)
	g.Get("/performers", h.auth, h.Performers)
	g.Get("/learnings", h.auth, h.Learnings)
	g.Get("/posts", h.auth, h.ListPosts)
	g.Get("/posts/:post_id", h.auth, h.PostDetail)
	g.Get("/best-times", h.auth, h.BestTimes)
	g.Get("/content-decay", h.auth, h.ContentDecay)
	g.Get("/posting-frequency", h.auth, h.PostingFrequency)
	g.Get("/followers", h.auth, h.Followers)
}

// validAnalyticsSorts is the closed set of sort_by values (CON-93 §5).
var validAnalyticsSorts = map[string]bool{
	"engagement": true, "impressions": true, "reach": true, "likes": true,
	"comments": true, "shares": true, "saves": true, "clicks": true,
	"views": true, "published_at": true,
}

type analyticsPagination struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
	Pages int `json:"pages"`
}

type analyticsListResponse struct {
	Items      []repository.PostAnalyticsListItem `json:"items"`
	Pagination analyticsPagination                `json:"pagination"`
	Overview   repository.PostAnalyticsOverview   `json:"overview"`
}

// ListPosts godoc
// @Summary      List post analytics
// @Description  Paged, sortable overview of analytics for Zernio-published posts, plus summed/averaged totals. Served entirely from the database — no publisher call on the request path (CON-93 FR5).
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        page      query     int     false  "Page (default 1)"
// @Param        limit     query     int     false  "Page size (default 50, max 100)"
// @Param        sort_by   query     string  false  "engagement|impressions|reach|likes|comments|shares|saves|clicks|views|published_at"
// @Param        order     query     string  false  "asc|desc (default desc)"
// @Param        platform  query     string  false  "Optional platform name filter"
// @Success      200  {object}  handlers.analyticsListResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/analytics/posts [get]
func (h *AnalyticsHandler) ListPosts(c *fiber.Ctx) error {
	// Fail-open when analytics is disabled (nil repo — ANALYTICS_DSN unset),
	// mirroring the per-post endpoint's 503 (CON-125 Track B).
	if h.repo == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "analytics is not available")
	}

	page := 1
	if raw := c.Query("page"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return fiber.NewError(fiber.StatusBadRequest, "page must be a positive integer")
		}
		page = v
	}

	limit := 50
	if raw := c.Query("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return fiber.NewError(fiber.StatusBadRequest, "limit must be a positive integer")
		}
		limit = v
	}
	if limit > 100 {
		limit = 100
	}

	sortBy := c.Query("sort_by", "engagement")
	if !validAnalyticsSorts[sortBy] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid sort_by")
	}
	order := c.Query("order", "desc")
	if order != "asc" && order != "desc" {
		return fiber.NewError(fiber.StatusBadRequest, "order must be asc or desc")
	}

	items, overview, err := h.repo.List(reqCtx(c), repository.PostAnalyticsListOptions{
		// Restricted to Zernio-published posts (CON-93 §5/§6). The marker
		// lives in models so this stays decoupled from the zernio adapter.
		Publisher: models.PublisherZernio,
		Platform:  c.Query("platform"),
		Page:      page,
		Limit:     limit,
		SortBy:    sortBy,
		Order:     order,
	})
	if err != nil {
		return err
	}

	total := overview.PostCount
	pages := 0
	if total > 0 {
		pages = (total + limit - 1) / limit
	}

	return c.JSON(analyticsListResponse{
		Items: items,
		Pagination: analyticsPagination{
			Page:  page,
			Limit: limit,
			Total: total,
			Pages: pages,
		},
		Overview: overview,
	})
}
