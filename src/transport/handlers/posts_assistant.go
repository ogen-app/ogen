package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PostAssistantHandler owns the post AI-assistant surface — the streaming
// assistant turn (POST /:id/assistant, CON-128) and the conversation history
// read (GET /:id/messages). Split out of the PostsHandler god-object (CON-291):
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
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err := validate.Struct(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, validationError(err).Error())
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	h.activity.Record(reqCtx(c), activity.CategoryAIFlow, "post_assistant_turn",
		activity.WithEntity("post", c.Params("id")),
		activity.WithSource(activity.SourceAssistant),
	)

	// c.Params / BodyParser values alias fasthttp's request buffer, which is
	// recycled for a later request the moment this handler returns — but the
	// StreamWriter below runs *after* that return and only persists at the end
	// of a multi-minute model call. Without copying, a concurrent request can
	// overwrite the buffer mid-turn, corrupting the post id (observed in the
	// wild as post_id="ations/zerni") so the final insert trips the post_id FK
	// and is mis-surfaced as "this post was deleted while the assistant was
	// working". Clone the retained strings to pin their own backing arrays.
	postID := strings.Clone(c.Params("id"))
	instruction := strings.Clone(req.Instruction)
	assistant := h.assistant
	// Carry the tenant into the detached flow context (the StreamWriter runs
	// after this handler returns) so usage recording + enforcement attribute
	// to the right tenant (CON-86).
	tenantID, _ := c.Locals(tenantctx.Key).(string)
	flowCtx := detachedContext(c, tenantID)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		writeEvent := func(event string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			_ = w.Flush()
		}

		onEvent := post_assistant.OnEventFunc(func(name post_assistant.SSEEventKind, data any) {
			writeEvent(string(name), data)
		})

		_, err := assistant(flowCtx, post_assistant.PostAssistantRequest{
			PostID:      postID,
			Instruction: instruction,
		}, onEvent)
		if err != nil {
			code := fiber.StatusInternalServerError
			msg := err.Error()
			var ve *post_assistant.ValidationError
			var ae *post_assistant.AIError
			switch {
			case errors.As(err, &ve):
				code = fiber.StatusBadRequest
				msg = ve.Msg
			case errors.As(err, &ae):
				code = fiber.StatusBadGateway
				msg = ae.Msg
			}
			writeEvent(string(post_assistant.SSEEventError), post_assistant.ErrorEventPayload{Message: msg, Code: code})
			return
		}
		// "complete" is emitted by the runner itself; nothing to write here.
	}))

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
