package handlers

import (
	"context"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// AssistantProposalsHandler answers assistant offers from their cards: the
// same work as a "yes" or "no" in the chat, without a model call.
type AssistantProposalsHandler struct {
	answer   func(ctx context.Context, req assistant.AnswerRequest, onEvent assistant.OnEventFunc) (*assistant.Response, error)
	activity *activity.Recorder
	auth     fiber.Handler
}

// NewAssistantProposalsHandler wires the proposal endpoints. answer nil leaves
// them at 503; activity nil is a no-op.
func NewAssistantProposalsHandler(
	answer func(ctx context.Context, req assistant.AnswerRequest, onEvent assistant.OnEventFunc) (*assistant.Response, error),
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *AssistantProposalsHandler {
	return &AssistantProposalsHandler{answer: answer, activity: activityRec, auth: auth}
}

func (h *AssistantProposalsHandler) Register(app *fiber.App) {
	g := app.Group("/api/assistant/proposals")
	g.Post("/:id/confirm", h.auth, h.Confirm)
	g.Post("/:id/dismiss", h.auth, h.Dismiss)
}

type confirmProposalRequest struct {
	// OptionID is the option the user picked, when the offer had options.
	OptionID string `json:"optionId"`
}

// Confirm godoc
// @Summary      Confirm an assistant offer (SSE)
// @Description  Runs an offer the assistant proposed, exactly as proposed, and streams the work's events
// @Description  like a chat turn: the work's own events, "proposal_resolved", then "complete" with a
// @Description  assistant.Response. "error" carries {"message":"...","code":<http_code>}: 404 when the
// @Description  offer does not exist, 409 when it was already answered, expired, names an unknown option, or
// @Description  the work itself was refused.
// @Tags         assistant
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id    path  string                  true   "Proposal id"
// @Param        body  body  confirmProposalRequest  false  "Picked option"
// @Success      200  "SSE stream: work events / proposal_resolved / complete / error"
// @Failure      401  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/assistant/proposals/{id}/confirm [post]
func (h *AssistantProposalsHandler) Confirm(c *fiber.Ctx) error {
	if h.answer == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "assistant is not available")
	}
	var req confirmProposalRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	h.activity.Record(reqCtx(c), activity.CategoryAIFlow, "assistant_proposal_confirmed",
		activity.WithEntity("assistant_proposal", c.Params("id")),
		activity.WithSource(activity.SourceAssistant),
	)

	// The stream writer outlives the request buffer; copy what it reads.
	answerReq := assistant.AnswerRequest{
		ProposalID: strings.Clone(c.Params("id")),
		OptionID:   strings.Clone(req.OptionID),
		Confirm:    true,
	}
	answer := h.answer
	tenantID, _ := c.Locals(tenantctx.Key).(string)
	flowCtx := detachedContext(c, tenantID)
	streamFlow(c, func(emit sseEmit) {
		_, err := answer(flowCtx, answerReq, func(name assistant.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(assistant.SSEEventError), proposalError(err))
		}
	})
	return nil
}

// Dismiss godoc
// @Summary      Dismiss an assistant offer
// @Description  Withdraws an offer the assistant proposed. Nothing runs. Returns the turn's
// @Description  assistant.Response (action "answered").
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Proposal id"
// @Success      200  {object}  map[string]any
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/assistant/proposals/{id}/dismiss [post]
func (h *AssistantProposalsHandler) Dismiss(c *fiber.Ctx) error {
	if h.answer == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "assistant is not available")
	}
	resp, err := h.answer(reqCtx(c), assistant.AnswerRequest{ProposalID: c.Params("id")}, nil)
	if err != nil {
		p := proposalError(err)
		return fiber.NewError(p.Code, p.Message)
	}
	return c.JSON(resp)
}

// proposalError maps an answer failure to the error a client can act on.
func proposalError(err error) flowErrorPayload {
	if errors.Is(err, assistant.ErrProposalNotFound) {
		return flowErrorPayload{Message: "This offer no longer exists.", Code: fiber.StatusNotFound}
	}
	if ce, ok := errors.AsType[*assistant.ProposalConflictError](err); ok {
		return flowErrorPayload{Message: ce.Msg, Code: fiber.StatusConflict}
	}
	return flowError[*assistant.ValidationError, *assistant.AIError](err)
}
