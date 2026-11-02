package handlers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PostInsightsHandler owns a post's read/derived-insight endpoints — quality
// assessment (POST /:id/assess + GET /:id/assessment) and the per-post
// analytics snapshot (GET /:id/analytics). Split out of the PostsHandler
// god-object: a focused handler carrying only the (optional) deps these
// read paths need, each nil-disabling its endpoint with a 503.
type PostInsightsHandler struct {
	repo             repository.PostRepository
	assessQuality    func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error)
	evaluationRepo   repository.PostEvaluationRepository
	qualityStatus    func(ctx context.Context, postID string) (*post_quality.Status, error)
	analyticsRepo    repository.PostAnalyticsRepository
	isAssistantReady func() bool
	activity         *activity.Recorder
	auth             fiber.Handler
}

// NewPostInsightsHandler wires the post insight endpoints. assessQuality /
// evaluationRepo / analyticsRepo are each optional — a nil one leaves its
// endpoint at 503. isAssistantReady nil means "always ready"; activity nil is a
// no-op.
func NewPostInsightsHandler(
	repo repository.PostRepository,
	assessQuality func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error),
	evaluationRepo repository.PostEvaluationRepository,
	analyticsRepo repository.PostAnalyticsRepository,
	isAssistantReady func() bool,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *PostInsightsHandler {
	return &PostInsightsHandler{
		repo:             repo,
		assessQuality:    assessQuality,
		evaluationRepo:   evaluationRepo,
		analyticsRepo:    analyticsRepo,
		isAssistantReady: isAssistantReady,
		activity:         activityRec,
		auth:             auth,
	}
}

// WithQualityStatus makes GET /:id/assessment report whether the stored
// assessment is stale and the post's run allowance. Without it, or while the
// flow is unavailable, the endpoint serves the stored row alone.
func (h *PostInsightsHandler) WithQualityStatus(fn func(ctx context.Context, postID string) (*post_quality.Status, error)) *PostInsightsHandler {
	h.qualityStatus = fn
	return h
}

func (h *PostInsightsHandler) Register(app *fiber.App) {
	app.Post("/api/posts/:id/assess", h.auth, h.Assess)
	app.Get("/api/posts/:id/assessment", h.auth, h.GetAssessment)
	app.Get("/api/posts/:id/analytics", h.auth, h.GetAnalytics)
}

// Assess godoc
// @Summary      Assess post quality
// @Description  Runs the Post quality assessment agent (CON-85) and streams
// @Description  Server-Sent Events: a "step" event per flow stage, then a
// @Description  final "complete" event carrying the persisted evaluation
// @Description  (four 0-10 dimension scores, each with a short verdict, at
// @Description  most five span-anchored suggestions across the post, plus the
// @Description  backend-computed overall). An "error" event with code 402
// @Description  means the post's quality_reviews_per_post allowance is used up.
// @Tags         posts
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {object}  post_quality.PostQualityResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/posts/{id}/assess [post]
func (h *PostInsightsHandler) Assess(c *fiber.Ctx) error {
	if h.assessQuality == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post quality assessment is not available")
	}
	if h.isAssistantReady != nil && !h.isAssistantReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post quality assessment is not available")
	}

	// Confirm the post exists before opening the SSE stream so a bad id
	// gets a clean 404 rather than an in-stream error event. Posts are
	// shared across the workspace — any authenticated user may assess any
	// post, consistent with GET/PUT/DELETE/assistant/clone.
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}

	postID := post.ID
	assess := h.assessQuality
	tenantID, _ := c.Locals(tenantctx.Key).(string)
	flowCtx := detachedContext(c, tenantID)
	// Captured now: the stream writer runs after the request context is
	// recycled, so the activity record can't read the actor from it.
	actor := actorID(c)
	rec := h.activity
	streamFlow(c, func(emit sseEmit) {
		_, err := assess(flowCtx, postID, func(name post_quality.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(post_quality.SSEEventError), assessError(err))
			return
		}
		// "complete" is emitted by the runner itself.
		rec.Record(flowCtx, activity.CategoryPost, "post_quality_assessed",
			activity.WithUser(actor),
			activity.WithEntity("post", postID),
			activity.WithSource(activity.SourceAPI),
			activity.WithStatus("success"),
		)
	})
	return nil
}

// assessError classifies an assessment failure for the SSE error event; an
// exhausted run allowance is 402, as the REST endpoints render it.
func assessError(err error) flowErrorPayload {
	if q, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return flowErrorPayload{Message: q.Error(), Code: fiber.StatusPaymentRequired}
	}
	return flowError[*post_quality.ValidationError, *post_quality.AIError](err)
}

// GetAssessment godoc
// @Summary      Get stored post quality assessment
// @Description  Returns the most recent persisted quality evaluation for a
// @Description  post without invoking the model. stale is true when the
// @Description  post's content or its context (brief, assets, model, rubric)
// @Description  changed since it was scored, and stale_reason says which;
// @Description  runs_used / runs_limit report the per-post allowance
// @Description  (runs_limit null = unlimited).
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {object}  post_quality.Status
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/posts/{id}/assessment [get]
func (h *PostInsightsHandler) GetAssessment(c *fiber.Ctx) error {
	if h.evaluationRepo == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post quality assessment is not available")
	}
	ctx, postID := reqCtx(c), c.Params("id")
	st, err := h.assessmentStatus(ctx, postID)
	if err != nil {
		return err
	}
	// A post that was never assessed is a 404, so the frontend shows the
	// "Assess" affordance rather than an empty result.
	if st == nil {
		return fiber.NewError(fiber.StatusNotFound, "post has not been assessed")
	}
	return c.JSON(st)
}

// assessmentStatus reads the stored assessment with its staleness, falling
// back to the stored row alone when the flow is not available.
func (h *PostInsightsHandler) assessmentStatus(ctx context.Context, postID string) (*post_quality.Status, error) {
	if h.qualityStatus != nil {
		st, err := h.qualityStatus(ctx, postID)
		if err == nil {
			return st, nil
		}
		slog.WarnContext(ctx, "assessment staleness unavailable; serving stored row", logging.AttrComponent, "handlers.posts", "post_id", postID, logging.AttrError, err)
	}
	eval, err := h.evaluationRepo.GetByPostID(ctx, postID)
	if err != nil || eval == nil {
		return nil, err
	}
	return &post_quality.Status{PostEvaluation: eval}, nil
}

// GetAnalytics godoc
// @Summary      Get a post's analytics snapshot
// @Description  Returns the latest engagement snapshot for a post published through a publisher. Served entirely from the database — never live-calls the publisher (CON-93 FR4). Two 200 shapes: a full snapshot, or — when the background refresh has not yet covered the post — the pending form `{"status":"pending","post_id":"..."}` so clients can poll. Both are covered by the schema below (the snapshot fields are absent on the pending response; `status` is absent on a snapshot).
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {object}  handlers.postAnalyticsResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/posts/{id}/analytics [get]
func (h *PostInsightsHandler) GetAnalytics(c *fiber.Ctx) error {
	if h.analyticsRepo == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post analytics is not available")
	}
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	// Analytics is defined only for posts published through a publisher;
	// today that's Zernio. 409 with a machine-readable code so
	// the client can distinguish "wrong kind of post" from "not found".
	if post.PublisherPostID == "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"code":  "not_published_via_publisher",
			"error": "post has not been published through a publisher",
		})
	}
	snapshot, err := h.analyticsRepo.GetByPostID(reqCtx(c), post.ID)
	if err != nil {
		return err
	}
	// No snapshot yet → the background refresh hasn't covered this post.
	// Return 200 pending (not 404) so clients can poll.
	if snapshot == nil {
		return c.JSON(fiber.Map{"status": "pending", "post_id": post.ID})
	}
	return c.JSON(newPostAnalyticsResponse(post, snapshot))
}
