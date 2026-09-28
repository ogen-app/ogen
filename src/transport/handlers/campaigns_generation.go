package handlers

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

// CampaignGenerationHandler owns the campaign AI generation/review SSE endpoints
// — targeted post generation (POST /:id/generate-posts, CON-114) and the
// read-only consistency reviews (POST /:id/brief-review + /:id/posts-review,
// CON-116). Split out of the CampaignsHandler god-object: a focused
// handler over the generate/review flow callbacks, each nil-disabling its
// endpoint. isContentPlanReady is the shared Anthropic-key readiness gate.
type CampaignGenerationHandler struct {
	repo               repository.CampaignRepository
	generatePosts      func(ctx context.Context, req content_plan.GeneratePostsRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
	generatePostsMax   int
	checkBrief         func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error)
	checkPosts         func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error)
	isContentPlanReady func() bool
	activity           *activity.Recorder
	auth               fiber.Handler
}

// NewCampaignGenerationHandler wires the generate/review endpoints. generatePosts
// / checkBrief / checkPosts are optional — a nil one leaves its endpoint at 503;
// isContentPlanReady nil means "always ready"; activity nil is a no-op.
func NewCampaignGenerationHandler(
	repo repository.CampaignRepository,
	generatePosts func(ctx context.Context, req content_plan.GeneratePostsRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error),
	generatePostsMax int,
	checkBrief func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error),
	checkPosts func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error),
	isContentPlanReady func() bool,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *CampaignGenerationHandler {
	return &CampaignGenerationHandler{
		repo:               repo,
		generatePosts:      generatePosts,
		generatePostsMax:   generatePostsMax,
		checkBrief:         checkBrief,
		checkPosts:         checkPosts,
		isContentPlanReady: isContentPlanReady,
		activity:           activityRec,
		auth:               auth,
	}
}

func (h *CampaignGenerationHandler) Register(app *fiber.App) {
	app.Post("/api/campaigns/:id/generate-posts", h.auth, h.GeneratePosts)
	app.Post("/api/campaigns/:id/brief-review", h.auth, h.BriefReview)
	app.Post("/api/campaigns/:id/posts-review", h.auth, h.PostsReview)
}

// recordActivity emits a best-effort CON-125 activity event with the API source
// pre-set. A private copy of the CampaignsHandler helper of the same name.
func (h *CampaignGenerationHandler) recordActivity(c *fiber.Ctx, category, typ string, opts ...activity.Option) {
	h.activity.Record(reqCtx(c), category, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

// GeneratePosts godoc
// @Summary      Generate targeted posts (SSE)
// @Description  Generates a few new draft posts for a platform subset, a single
// @Description  phase, and a publish-date window (CON-114). Streams the same
// @Description  step / post / warning / complete / error events as generate-draft.
// @Description  Available to any user in the campaign's tenant.
// @Tags         campaigns
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id    path  string  true  "Campaign Sqid"
// @Success      200  "SSE stream: step / post / warning / complete / error events"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/{id}/generate-posts [post]
func (h *CampaignGenerationHandler) GeneratePosts(c *fiber.Ctx) error {
	if h.generatePosts == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "targeted generation is not available")
	}
	if h.isContentPlanReady != nil && !h.isContentPlanReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "targeted generation is not available")
	}

	var body struct {
		PlatformIDs []string `json:"platformIds"`
		PhaseID     string   `json:"phaseId"`
		Count       int      `json:"count"`
		WindowStart string   `json:"windowStart"`
		WindowEnd   string   `json:"windowEnd"`
		PostType    string   `json:"postType"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if body.Count < 1 {
		return fiber.NewError(fiber.StatusBadRequest, "count must be at least 1")
	}
	limit := h.generatePostsMax
	if limit <= 0 {
		limit = 10
	}
	body.Count = min(body.Count, limit)

	// 404 before opening the stream (tenant-scoped).
	if _, err := load(c, h.repo.GetByID, "campaign not found"); err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	h.recordActivity(c, activity.CategoryCampaign, "content_generated",
		activity.WithEntity("campaign", c.Params("id")),
		activity.WithPayload(map[string]any{"mode": "generate_posts", "count": body.Count}),
	)

	flowCtx := detachedContext(c, session.TenantID)
	generatePosts := h.generatePosts
	req := content_plan.GeneratePostsRequest{
		// Cloned: the stream writer outlives the request buffer this id aliases.
		CampaignID:  strings.Clone(c.Params("id")),
		PlatformIDs: body.PlatformIDs,
		PhaseID:     body.PhaseID,
		Count:       body.Count,
		WindowStart: body.WindowStart,
		WindowEnd:   body.WindowEnd,
		PostType:    body.PostType,
	}
	streamFlow(c, func(emit sseEmit) {
		resp, err := generatePosts(flowCtx, req, func(name content_plan.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(content_plan.SSEEventError), flowError[*content_plan.ValidationError, *content_plan.AIError](err))
			return
		}
		emit(string(content_plan.SSEEventComplete), resp)
	})
	return nil
}

// BriefReview godoc
// @Summary      Review the campaign brief for consistency (SSE)
// @Description  Read-only review of the brief's internal consistency and
// @Description  completeness (CON-116). Streams step / complete / error events.
// @Description  Does not modify the brief. Available to any user in the tenant.
// @Tags         campaigns
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id   path  string  true  "Campaign Sqid"
// @Success      200  "SSE stream: step / complete / error events"
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/{id}/brief-review [post]
func (h *CampaignGenerationHandler) BriefReview(c *fiber.Ctx) error {
	if h.checkBrief == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "brief review is not available")
	}
	if h.isContentPlanReady != nil && !h.isContentPlanReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "brief review is not available")
	}

	// 404 before opening the stream (tenant-scoped).
	if _, err := load(c, h.repo.GetByID, "campaign not found"); err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	flowCtx := detachedContext(c, session.TenantID)
	h.recordActivity(c, activity.CategoryAIFlow, "brief_review",
		activity.WithEntity("campaign", c.Params("id")),
	)

	checkBrief := h.checkBrief
	// Cloned: the stream writer outlives the request buffer.
	campaignID := strings.Clone(c.Params("id"))
	streamFlow(c, func(emit sseEmit) {
		resp, err := checkBrief(flowCtx, campaignID, consistencyEmitter(emit))
		if err != nil {
			emit(string(consistency.SSEEventError), consistencyError(err))
			return
		}
		emit(string(consistency.SSEEventComplete), resp)
	})
	return nil
}

// PostsReview godoc
// @Summary      Review campaign posts against the brief (SSE)
// @Description  Read-only check of whether the campaign's non-published posts
// @Description  follow the brief (CON-116). Only the first N posts are checked.
// @Description  Streams step / complete / error events. Does not modify any post.
// @Tags         campaigns
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id   path  string  true  "Campaign Sqid"
// @Success      200  "SSE stream: step / complete / error events"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/{id}/posts-review [post]
func (h *CampaignGenerationHandler) PostsReview(c *fiber.Ctx) error {
	if h.checkPosts == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "posts review is not available")
	}
	if h.isContentPlanReady != nil && !h.isContentPlanReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "posts review is not available")
	}

	var body struct {
		Max int `json:"max"`
	}
	// Body is optional — an empty POST reviews with the default cap.
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&body); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}

	// 404 before opening the stream (tenant-scoped).
	if _, err := load(c, h.repo.GetByID, "campaign not found"); err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	flowCtx := detachedContext(c, session.TenantID)
	h.recordActivity(c, activity.CategoryAIFlow, "posts_review",
		activity.WithEntity("campaign", c.Params("id")),
	)

	checkPosts := h.checkPosts
	// Cloned: the stream writer outlives the request buffer.
	req := consistency.PostsCheckRequest{CampaignID: strings.Clone(c.Params("id")), Max: body.Max}
	streamFlow(c, func(emit sseEmit) {
		resp, err := checkPosts(flowCtx, req, consistencyEmitter(emit))
		if err != nil {
			emit(string(consistency.SSEEventError), consistencyError(err))
			return
		}
		emit(string(consistency.SSEEventComplete), resp)
	})
	return nil
}

// consistencyEmitter adapts an SSE emitter to the consistency flow callback.
func consistencyEmitter(emit sseEmit) consistency.OnEventFunc {
	return func(name consistency.SSEEventKind, data any) { emit(string(name), data) }
}

// consistencyError maps a consistency flow error onto the SSE error payload.
func consistencyError(err error) flowErrorPayload {
	return flowError[*consistency.ValidationError, *consistency.AIError](err)
}
