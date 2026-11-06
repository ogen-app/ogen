package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// threadMessages is how many of a thread's latest messages the history read
// returns.
const threadMessages = 50

// AssistantHandler serves the universal assistant: one endpoint for every
// screen, and the thread history.
type AssistantHandler struct {
	run       func(ctx context.Context, req assistant.Request, onEvent assistant.OnEventFunc) (*assistant.Response, error)
	available func() bool
	resolve   func(ctx context.Context, v *viewcontext.ViewContext) (*viewresolve.Resolved, error)
	messages  repository.AssistantMessageRepository
	activity  *activity.Recorder
	auth      fiber.Handler
}

// NewAssistantHandler wires the assistant endpoints. run nil, or available
// reporting false, leaves the turn endpoint at 503; activity nil is a no-op.
func NewAssistantHandler(
	run func(ctx context.Context, req assistant.Request, onEvent assistant.OnEventFunc) (*assistant.Response, error),
	available func() bool,
	resolve func(ctx context.Context, v *viewcontext.ViewContext) (*viewresolve.Resolved, error),
	messages repository.AssistantMessageRepository,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *AssistantHandler {
	return &AssistantHandler{run: run, available: available, resolve: resolve, messages: messages, activity: activityRec, auth: auth}
}

func (h *AssistantHandler) Register(app *fiber.App) {
	app.Post("/api/assistant", h.auth, h.Turn)
	app.Get("/api/assistant/threads/:scope/messages", h.auth, h.Messages)
}

type assistantTurnRequest struct {
	Instruction string `json:"instruction" validate:"required"`
	// ViewContext is the screen the message is sent from; its scope names the
	// thread. See domain/viewcontext for the schema.
	ViewContext json.RawMessage `json:"viewContext" validate:"required" swaggertype:"object"`
}

// Turn godoc
// @Summary      Universal assistant turn (SSE)
// @Description  Sends a message to the assistant from any screen and streams the reply. viewContext
// @Description  (version 1) is the screen: screen, scope ({kind: workspace|campaign|post, id}), focus,
// @Description  selection ({kind, ids | allMatching}), filter, savedViewId, sort, window and visible. It holds
// @Description  references only; the server reloads them in the caller's workspace and drops ids that no
// @Description  longer exist. The scope names the thread: the conversation continues across the screens of
// @Description  one workspace, campaign or post. A malformed viewContext is a 400 that names the field; a
// @Description  campaign or post scope that doesn't exist is a 404. Events: "explanation_delta" {"delta"},
// @Description  "tool_call", "tool_result", then "complete" with {"explanation","action"}. "error" carries
// @Description  {"message":"...","code":<http_code>}.
// @Tags         assistant
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        body  body  assistantTurnRequest  true  "Message and screen"
// @Success      200  "SSE stream: explanation_delta / tool_call / tool_result / complete / error"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/assistant [post]
func (h *AssistantHandler) Turn(c *fiber.Ctx) error {
	if h.run == nil || (h.available != nil && !h.available()) {
		return fiber.NewError(fiber.StatusServiceUnavailable, "assistant is not available")
	}
	var req assistantTurnRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	view, err := viewcontext.Parse(req.ViewContext)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	// The view resolves before the stream opens, so a bad reference is a
	// plain status code rather than an error event.
	resolved, err := h.resolve(reqCtx(c), view)
	switch {
	case errors.Is(err, viewresolve.ErrScopeNotFound):
		return fiber.NewError(fiber.StatusNotFound, string(view.Scope.Kind)+" not found")
	case err != nil:
		if ie, ok := errors.AsType[*viewcontext.InvalidError](err); ok {
			return fiber.NewError(fiber.StatusBadRequest, ie.Error())
		}
		return err
	}

	h.activity.Record(reqCtx(c), activity.CategoryAIFlow, "assistant_turn",
		activity.WithEntity(string(view.Scope.Kind), view.Scope.ID),
		activity.WithSource(activity.SourceAssistant),
	)

	// The stream writer outlives the request buffer; copy what it reads. The
	// view was decoded into fresh strings already.
	turn := assistant.Request{Instruction: strings.Clone(req.Instruction), View: resolved}
	run := h.run
	flowCtx := detachedContext(c, session.TenantID)
	streamFlow(c, func(emit sseEmit) {
		_, err := run(flowCtx, turn, func(kind assistant.SSEEventKind, data any) {
			emit(string(kind), data)
		})
		if err != nil {
			emit(string(assistant.SSEEventError), flowError[*assistant.ValidationError, *assistant.AIError](err))
		}
	})
	return nil
}

// Messages godoc
// @Summary      Universal assistant thread
// @Description  Returns a thread's latest messages, oldest first. scope is "workspace", "campaign:<id>" or
// @Description  "post:<id>". A user message carries the view_context it was sent from; a model message's
// @Description  content is the JSON of that turn's "complete" event.
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        scope  path  string  true  "workspace | campaign:<id> | post:<id>"
// @Success      200  {array}   models.AssistantMessage
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/assistant/threads/{scope}/messages [get]
func (h *AssistantHandler) Messages(c *fiber.Ctx) error {
	kind, id, err := parseThreadScope(c.Params("scope"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	msgs, err := h.messages.ListRecent(reqCtx(c), kind, id, threadMessages)
	if err != nil {
		return err
	}
	if msgs == nil {
		msgs = []models.AssistantMessage{}
	}
	return c.JSON(msgs)
}

// parseThreadScope reads "workspace", "campaign:<id>" or "post:<id>".
func parseThreadScope(s string) (models.AssistantScopeKind, string, error) {
	kind, id, _ := strings.Cut(s, ":")
	switch models.AssistantScopeKind(kind) {
	case models.AssistantScopeWorkspace:
		if id == "" {
			return models.AssistantScopeWorkspace, "", nil
		}
	case models.AssistantScopeCampaign, models.AssistantScopePost:
		if id != "" && len(id) <= viewcontext.MaxIDLen {
			return models.AssistantScopeKind(kind), id, nil
		}
	}
	return "", "", errors.New(`scope must be "workspace", "campaign:<id>" or "post:<id>"`)
}
