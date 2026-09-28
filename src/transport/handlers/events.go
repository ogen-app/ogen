package handlers

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// EventsHandler streams Hub events to authenticated clients over SSE.
type EventsHandler struct {
	hub               eventhub.Hub
	sessionRepo       repository.SessionRepository
	auth              fiber.Handler
	heartbeatInterval time.Duration
	maxLifetime       time.Duration
}

// NewEventsHandler wires the SSE stream endpoint. heartbeatInterval = 0
// uses the package default (20s); tests pass a smaller value.
func NewEventsHandler(
	hub eventhub.Hub,
	sessionRepo repository.SessionRepository,
	auth fiber.Handler,
	heartbeatInterval time.Duration,
) *EventsHandler {
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	return &EventsHandler{
		hub:               hub,
		sessionRepo:       sessionRepo,
		auth:              auth,
		heartbeatInterval: heartbeatInterval,
		maxLifetime:       defaultStreamLifetime,
	}
}

// SetMaxLifetime overrides the per-connection lifetime ceiling.
// A non-positive value is ignored. Primarily for tests, which use a short
// lifetime to drive the reclamation path deterministically.
func (h *EventsHandler) SetMaxLifetime(d time.Duration) {
	if d > 0 {
		h.maxLifetime = d
	}
}

func (h *EventsHandler) Register(app *fiber.App) {
	app.Get("/api/events", h.auth, h.Stream)
}

// Stream godoc
// @Summary      Event stream (SSE)
// @Description  Subscribes the authenticated user to a long-lived
// @Description  Server-Sent Events stream filtered by topic patterns.
// @Description
// @Description  Topic filters are passed via the `topics` query param as a
// @Description  comma-separated list. Supported forms per filter:
// @Description    - "all"               — receive every event the user is
// @Description                            authorised to see.
// @Description    - "kind:id"           — exact match (e.g. "entity:post:Xq").
// @Description    - "kind:*"            — prefix wildcard (e.g. "job:*").
// @Description
// @Description  Each event is emitted as one SSE frame:
// @Description    id: <event-id>
// @Description    event: <type>
// @Description    data: <json>
// @Description
// @Description  A heartbeat comment (`: ping`) is sent every 20 seconds.
// @Description  Delivery is at-most-once; the server holds no event log.
// @Description  Subscribers are disconnected if their per-connection buffer
// @Description  overflows (the client should reconnect and reconcile via REST).
// @Description
// @Description  The `Last-Event-ID` request header is currently accepted
// @Description  and ignored — reserved for future replay support.
// @Tags         events
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        topics  query  string  true  "Comma-separated topic filters"
// @Success      200  "SSE stream"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      429  {object}  map[string]string
// @Router       /api/events [get]
func (h *EventsHandler) Stream(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	topics, err := parseTopicsParam(c.Query("topics"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	eventCh, unsubscribe, err := h.hub.Subscribe(reqCtx(c), eventhub.SubscribeOpts{
		UserID:   session.UserID,
		TenantID: session.TenantID, // Only this tenant's events
		Topics:   topics,
	})
	if err != nil {
		if errors.Is(err, eventhub.ErrTooManySubscribers) {
			return fiber.NewError(fiber.StatusTooManyRequests, err.Error())
		}
		if errors.Is(err, eventhub.ErrNoTopics) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}

	streamHub(c, session, hubStream{
		component:      "events",
		events:         eventCh,
		unsubscribe:    unsubscribe,
		sessionRepo:    h.sessionRepo,
		heartbeat:      h.heartbeatInterval,
		lifetime:       h.maxLifetime,
		writeFailedMsg: "sse write failed",
		sessionGoneMsg: "session no longer valid; closing stream",
		write:          writeSSEEvent,
	})
	return nil
}

// parseTopicsParam splits and trims a comma-separated topics query value.
// Returns ErrNoTopics if no usable topic remains after trimming.
func parseTopicsParam(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("topics query param is required (e.g. ?topics=all or ?topics=job:*,entity:post:abc)")
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("topics query param must contain at least one non-empty topic")
	}
	return out, nil
}

// writeSSEEvent emits one well-formed SSE frame. JSON is written on a
// single `data:` line — the events the Hub carries are flat enough that
// multi-line data isn't needed today.
func writeSSEEvent(w *bufio.Writer, ev eventhub.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	eventType := ev.Type
	if eventType == "" {
		eventType = "message" // SSE default; helps EventSource consumers
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, eventType, body); err != nil {
		return err
	}
	return w.Flush()
}
