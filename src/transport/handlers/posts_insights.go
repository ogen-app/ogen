package handlers

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PostInsightsHandler owns a post's read/derived-insight endpoints — quality
// assessment (CON-85: POST /:id/assess + GET /:id/assessment) and the per-post
// analytics snapshot (CON-93: GET /:id/analytics). Split out of the PostsHandler
// god-object (CON-291): a focused handler carrying only the (optional) deps these
// read paths need, each nil-disabling its endpoint with a 503.
type PostInsightsHandler struct {
	repo             repository.PostRepository
	assessQuality    func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error)
	evaluationRepo   repository.PostEvaluationRepository
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
// @Description  (four 0-10 dimension scores with rationale, weakness, and
// @Description  span-anchored suggestions, plus the backend-computed overall).
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
	post, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		return err
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	postID := post.ID
	assess := h.assessQuality
	tenantID, _ := c.Locals(tenantctx.Key).(string)
	flowCtx := detachedContext(c, tenantID)
	// Capture the actor now; the stream writer runs after the request context
	// may be recycled, so the activity record can't read it from c.Context().
	var actorID string
	if sess, ok := c.Locals("session").(*models.Session); ok && sess != nil {
		actorID = sess.UserID
	}

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		writeEvent := func(event string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			_ = w.Flush()
		}

		onEvent := post_quality.OnEventFunc(func(name post_quality.SSEEventKind, data any) {
			writeEvent(string(name), data)
		})

		_, err := assess(flowCtx, postID, onEvent)
		if err != nil {
			code := fiber.StatusInternalServerError
			msg := err.Error()
			var ve *post_quality.ValidationError
			var ae *post_quality.AIError
			switch {
			case errors.As(err, &ve):
				code = fiber.StatusBadRequest
				msg = ve.Msg
			case errors.As(err, &ae):
				code = fiber.StatusBadGateway
				msg = ae.Msg
			}
			writeEvent(string(post_quality.SSEEventError), post_quality.ErrorEventPayload{Message: msg, Code: code})
			return
		}
		// "complete" is emitted by the runner itself; nothing to write here.
		h.activity.Record(flowCtx, activity.CategoryPost, "post_quality_assessed",
			activity.WithUser(actorID),
			activity.WithEntity("post", postID),
			activity.WithSource(activity.SourceAPI),
			activity.WithStatus("success"),
		)
	}))

	return nil
}

// GetAssessment godoc
// @Summary      Get stored post quality assessment
// @Description  Returns the most recent persisted quality evaluation for a
// @Description  post (CON-85/CON-92) without invoking the model. The frontend
// @Description  reads this to render an existing assessment and only triggers
// @Description  POST /assess when the post has changed since it was scored.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {object}  models.PostEvaluation
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/posts/{id}/assessment [get]
func (h *PostInsightsHandler) GetAssessment(c *fiber.Ctx) error {
	if h.evaluationRepo == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post quality assessment is not available")
	}
	eval, err := h.evaluationRepo.GetByPostID(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	// GetByPostID returns (nil, nil) when the post has never been assessed.
	// Surface that as 404 so the frontend shows the "Assess" affordance
	// rather than a stale or empty result.
	if eval == nil {
		return fiber.NewError(fiber.StatusNotFound, "post has not been assessed")
	}
	return c.JSON(eval)
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
	post, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		return err
	}
	// Analytics is defined only for posts published through a publisher;
	// today that's Zernio (CON-93 §9). 409 with a machine-readable code so
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
	// Return 200 pending (not 404) so clients can poll (CON-93 §10).
	if snapshot == nil {
		return c.JSON(fiber.Map{"status": "pending", "post_id": post.ID})
	}
	return c.JSON(newPostAnalyticsResponse(post, snapshot))
}
