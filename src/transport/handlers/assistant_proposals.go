package handlers

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// AssistantProposalsHandler answers assistant offers from their cards: the
// same work as a "yes" or "no" in the chat, without a model call.
type AssistantProposalsHandler struct {
	answer func(ctx context.Context, req assistant.AnswerRequest, onEvent assistant.OnEventFunc) (*assistant.Response, error)
	// available reports whether the assistant can answer right now.
	available func() bool
	activity  *activity.Recorder
	auth      fiber.Handler
}

// NewAssistantProposalsHandler wires the proposal endpoints. answer nil, or
// available reporting false, leaves them at 503; activity nil is a no-op.
func NewAssistantProposalsHandler(
	answer func(ctx context.Context, req assistant.AnswerRequest, onEvent assistant.OnEventFunc) (*assistant.Response, error),
	available func() bool,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *AssistantProposalsHandler {
	return &AssistantProposalsHandler{answer: answer, available: available, activity: activityRec, auth: auth}
}

// Register mounts the confirm and dismiss endpoints.
func (h *AssistantProposalsHandler) Register(app *fiber.App) {
	g := app.Group("/api/assistant/proposals")
	g.Post("/:id/confirm", h.auth, h.Confirm)
	g.Post("/:id/dismiss", h.auth, h.Dismiss)
}

type confirmProposalRequest struct {
	// OptionID is the option the user picked, when the offer had options.
	OptionID string `json:"optionId"`
	// Input is the value the picked option asks for (its card's "input"),
	// e.g. the link of the page to import.
	Input string `json:"input,omitempty"`
}

// maxOptionInputLen bounds the value typed into an option's field.
const maxOptionInputLen = 2048

// Confirm godoc
// @Summary      Confirm an assistant offer (SSE)
// @Description  Runs an offer the assistant proposed, exactly as proposed, and streams the work's events
// @Description  like a chat turn: the work's own events, "proposal_created" for any fresh offer, "proposal_resolved",
// @Description  then "complete" with a assistant.Response. When what the offer was made on changed since, nothing
// @Description  runs and the offer stays open: "proposal_resolved" says status "stale" with "reasons" (what
// @Description  changed, in plain words) and, when what still holds is offered afresh, "replacedBy" naming that
// @Description  offer, whose card has "replaces"; the result carries the same as "changes". From then on only the
// @Description  fresh offer can be confirmed. A run that did part of the job says so; its result lists "notMade"
// @Description  and, when some items are worth retrying, an optional "retryProposalId" offering just those, whose
// @Description  card has "retryOf". An option whose card has "input" (e.g. {"kind":"url","label":"Page link"})
// @Description  runs with the typed value sent as "input".
// @Description  "error" carries {"message":"...","code":<http_code>,"reason":<optional>}: 404 when the offer does
// @Description  not exist, 409 when it can't be answered (reason "offer_expired" — "proposal_resolved" says
// @Description  "expired" the first time —, "offer_answered", or "offer_replaced" when a fresh offer replaced it),
// @Description  names an unknown option, picks an option that asks for input without it (reason
// @Description  "input_required"), or the work itself was refused (no reason; the message says why).
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
	if !h.ready() {
		return fiber.NewError(fiber.StatusServiceUnavailable, errAssistantUnavailable)
	}
	var req confirmProposalRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	if utf8.RuneCountInString(req.Input) > maxOptionInputLen {
		return fiber.NewError(fiber.StatusBadRequest, "input is too long")
	}
	// The stream writer outlives the request buffer; copy what it reads.
	answerReq := assistant.AnswerRequest{
		ProposalID: strings.Clone(c.Params("id")),
		OptionID:   strings.Clone(req.OptionID),
		Input:      strings.Clone(req.Input),
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
			emit(string(assistant.SSEEventError), h.proposalError(err))
			return
		}
		h.activity.Record(flowCtx, activity.CategoryAIFlow, "assistant_proposal_confirmed",
			activity.WithEntity("assistant_proposal", answerReq.ProposalID),
			activity.WithSource(activity.SourceAssistant),
		)
	})
	return nil
}

// Dismiss godoc
// @Summary      Dismiss an assistant offer
// @Description  Withdraws an offer the assistant proposed. Nothing runs. Returns the turn's
// @Description  assistant.Response (action "answered"). A 409 for an offer that can't be answered is
// @Description  {"error":"...","reason":"offer_expired"|"offer_answered"}.
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
	if !h.ready() {
		return fiber.NewError(fiber.StatusServiceUnavailable, errAssistantUnavailable)
	}
	resp, err := h.answer(reqCtx(c), assistant.AnswerRequest{ProposalID: c.Params("id")}, nil)
	if err != nil {
		p := h.proposalError(err)
		if p.Reason != "" {
			return c.Status(p.Code).JSON(fiber.Map{"error": p.Message, "reason": p.Reason})
		}
		return fiber.NewError(p.Code, p.Message)
	}
	return c.JSON(resp)
}

const errAssistantUnavailable = "assistant is not available"

// ready reports whether the endpoints can answer an offer now.
func (h *AssistantProposalsHandler) ready() bool {
	return h.answer != nil && (h.available == nil || h.available())
}

// proposalError maps an answer failure to the error a client can act on. An
// assistant that went away while answering is 503, not a server fault.
func (h *AssistantProposalsHandler) proposalError(err error) flowErrorPayload {
	if !h.ready() {
		return flowErrorPayload{Message: errAssistantUnavailable, Code: fiber.StatusServiceUnavailable}
	}
	if errors.Is(err, assistant.ErrProposalNotFound) {
		return flowErrorPayload{Message: "This offer no longer exists.", Code: fiber.StatusNotFound}
	}
	if ce, ok := errors.AsType[*assistant.ProposalConflictError](err); ok {
		return flowErrorPayload{Message: ce.Msg, Code: fiber.StatusConflict, Reason: ce.Code}
	}
	if msg, ok := workModelFailure(err); ok {
		return flowErrorPayload{Message: msg, Code: fiber.StatusBadGateway}
	}
	return flowError[*assistant.ValidationError, *assistant.AIError](err)
}

// workModelFailure reports a confirmed offer's work that failed at the model
// provider: the generation flows an offer runs return their own AIError,
// which reaches the answer unwrapped.
func workModelFailure(err error) (string, bool) {
	if e, ok := errors.AsType[*content_plan.AIError](err); ok {
		return e.Msg, true
	}
	if e, ok := errors.AsType[*draft_post.AIError](err); ok {
		return e.Msg, true
	}
	if e, ok := errors.AsType[*enrich_brief.AIError](err); ok {
		return e.Msg, true
	}
	return "", false
}
