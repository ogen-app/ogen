package handlers

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PostAssistantHandler owns the post AI-assistant surface — the streaming
// assistant turn (POST /:id/assistant, CON-128) and the conversation history
// read (GET /:id/messages). Split out of the PostsHandler god-object:
// a focused handler over the assistant runner + message repo, each nil-disabling
// its endpoint.
type PostAssistantHandler struct {
	assistant        func(ctx context.Context, req post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error)
	isAssistantReady func() bool
	messageRepo      repository.PostAssistantMessageRepository
	activity         *activity.Recorder
	auth             fiber.Handler
}

// NewPostAssistantHandler wires the assistant endpoints. assistant nil leaves
// POST /:id/assistant at 503; isAssistantReady nil means "always ready";
// activity nil is a no-op.
func NewPostAssistantHandler(
	assistant func(ctx context.Context, req post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error),
	isAssistantReady func() bool,
	messageRepo repository.PostAssistantMessageRepository,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *PostAssistantHandler {
	return &PostAssistantHandler{
		assistant:        assistant,
		isAssistantReady: isAssistantReady,
		messageRepo:      messageRepo,
		activity:         activityRec,
		auth:             auth,
	}
}

func (h *PostAssistantHandler) Register(app *fiber.App) {
	app.Post("/api/posts/:id/assistant", h.auth, h.Assistant)
	app.Get("/api/posts/:id/messages", h.auth, h.ListMessages)
}

type assistantRequest struct {
	Instruction string `json:"instruction" validate:"required"`
}

// Assistant godoc
// @Summary      Post assistant (SSE)
// @Description  Sends an instruction to the AI assistant and streams progress via Server-Sent Events.
// @Description  Events: "explanation_delta" and "content_delta" carry {"delta":"..."} fragments (Markdown)
// @Description  as the model generates the explanation and updated content. "tool_call" and "tool_result"
// @Description  signal asset-retrieval tool invocations. "complete" carries the final PostAssistantResponse
// @Description  whose updatedContent is a Markdown string.
// @Description  "error" carries {"message":"...","code":<http_code>}.
// @Tags         posts
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id    path      string           true  "Post Sqid"
// @Param        body  body      assistantRequest true  "Instruction payload"
// @Success      200  "SSE stream: delta / tool_call / tool_result / complete / error events"
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/posts/{id}/assistant [post]
func (h *PostAssistantHandler) Assistant(c *fiber.Ctx) error {
	if h.assistant == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post assistant is not available")
	}
	if h.isAssistantReady != nil && !h.isAssistantReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "post assistant is not available")
	}
	var req assistantRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	h.activity.Record(reqCtx(c), activity.CategoryAIFlow, "post_assistant_turn",
		activity.WithEntity("post", c.Params("id")),
		activity.WithSource(activity.SourceAssistant),
	)

	// c.Params / BodyParser values alias fasthttp's request buffer, which is
	// recycled for a later request the moment this handler returns — but the
	// stream writer runs after that return and only persists at the end of a
	// multi-minute model call. Without copying, a concurrent request can
	// overwrite the buffer mid-turn, corrupting the post id so the final
	// insert trips the post_id FK and is mis-surfaced as "this post was
	// deleted while the assistant was working".
	flowReq := post_assistant.PostAssistantRequest{
		PostID:      strings.Clone(c.Params("id")),
		Instruction: strings.Clone(req.Instruction),
	}
	assistant := h.assistant
	// The detached context carries the tenant so usage recording and
	// enforcement attribute correctly.
	tenantID, _ := c.Locals(tenantctx.Key).(string)
	flowCtx := detachedContext(c, tenantID)
	streamFlow(c, func(emit sseEmit) {
		_, err := assistant(flowCtx, flowReq, func(name post_assistant.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(post_assistant.SSEEventError), flowError[*post_assistant.ValidationError, *post_assistant.AIError](err))
		}
		// "complete" is emitted by the runner itself.
	})
	return nil
}

// ListMessages godoc
// @Summary      List assistant messages
// @Description  Returns the most recent assistant conversation messages for a post.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {array}   models.PostAssistantMessage
// @Failure      401  {object}  map[string]string
// @Router       /api/posts/{id}/messages [get]
func (h *PostAssistantHandler) ListMessages(c *fiber.Ctx) error {
	msgs, err := h.messageRepo.ListRecentByPostID(reqCtx(c), c.Params("id"), 50)
	if err != nil {
		return err
	}
	// Preserve the array contract: an empty history serializes as [] not null.
	if msgs == nil {
		msgs = []models.PostAssistantMessage{}
	}
	return c.JSON(msgs)
}
