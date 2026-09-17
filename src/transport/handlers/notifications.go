package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// maxReplayNotifications caps how many missed notifications one SSE reconnect
// replays from the durable log; a client further behind catches the rest via
// the REST list (?before=). Keeps a long-offline client from dumping unbounded
// rows onto the stream at once.
const maxReplayNotifications = 200

// NotificationsHandler serves the per-user notification inbox (CON-242): the
// REST read/write API plus the durable SSE stream. The stream reuses the shared
// eventhub for live push and the notifications table for reconnect replay.
type NotificationsHandler struct {
	repo              repository.NotificationRepository
	hub               eventhub.Hub
	sessionRepo       repository.SessionRepository
	auth              fiber.Handler
	heartbeatInterval time.Duration
	maxLifetime       time.Duration
}

// NewNotificationsHandler wires the inbox endpoints. heartbeatInterval = 0 uses
// the package default (20s); tests pass a smaller value.
func NewNotificationsHandler(
	repo repository.NotificationRepository,
	hub eventhub.Hub,
	sessionRepo repository.SessionRepository,
	auth fiber.Handler,
	heartbeatInterval time.Duration,
) *NotificationsHandler {
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	return &NotificationsHandler{
		repo:              repo,
		hub:               hub,
		sessionRepo:       sessionRepo,
		auth:              auth,
		heartbeatInterval: heartbeatInterval,
		maxLifetime:       defaultStreamLifetime,
	}
}

// SetMaxLifetime overrides the per-connection lifetime ceiling (CON-286).
// A non-positive value is ignored. Primarily for tests.
func (h *NotificationsHandler) SetMaxLifetime(d time.Duration) {
	if d > 0 {
		h.maxLifetime = d
	}
}

func (h *NotificationsHandler) Register(app *fiber.App) {
	g := app.Group("/api/notifications", h.auth)
	// Static routes before the parametric ones (CON-130) so /unread-count etc.
	// aren't shadowed by /:id.
	g.Get("/", h.List)
	g.Get("/unread-count", h.UnreadCount)
	g.Get("/stream", h.Stream)
	g.Post("/mark-all-read", h.MarkAllRead)
	g.Patch("/:id", h.Patch)
	g.Delete("/:id", h.Dismiss)
}

func (h *NotificationsHandler) session(c *fiber.Ctx) (*models.Session, error) {
	s, ok := c.Locals("session").(*models.Session)
	if !ok || s == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	return s, nil
}

// List godoc
// @Summary  List notifications
// @Tags     notifications
// @Produce  json
// @Security CookieAuth
// @Param    status query string false "unread|all (default all)"
// @Param    limit  query int    false "page size (default 30, max 100)"
// @Param    before query int    false "keyset cursor: seq to page before (older)"
// @Param    since  query int    false "only notifications newer than this seq"
// @Success  200 {array} models.Notification
// @Router   /api/notifications [get]
func (h *NotificationsHandler) List(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	before, err := seqParam(c, "before")
	if err != nil {
		return err
	}
	since, err := seqParam(c, "since")
	if err != nil {
		return err
	}
	limit, err := limitParam(c)
	if err != nil {
		return err
	}
	list, err := h.repo.List(c.Context(), s.UserID, repository.NotificationListOpts{
		UnreadOnly: c.Query("status") == "unread",
		Limit:      limit,
		BeforeSeq:  before,
		SinceSeq:   since,
	})
	if err != nil {
		return err
	}
	// Never return a null body for an empty list.
	if list == nil {
		list = []models.Notification{}
	}
	return c.JSON(list)
}

// UnreadCount godoc
// @Summary  Unread notification count (badge)
// @Tags     notifications
// @Produce  json
// @Security CookieAuth
// @Success  200 {object} map[string]int
// @Router   /api/notifications/unread-count [get]
func (h *NotificationsHandler) UnreadCount(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	n, err := h.repo.UnreadCount(c.Context(), s.UserID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"count": n})
}

type patchNotificationRequest struct {
	Read *bool `json:"read"`
}

// Patch godoc
// @Summary  Mark a notification read/unread
// @Tags     notifications
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id   path string                   true "Notification id"
// @Param    body body patchNotificationRequest true "read state"
// @Success  200 {object} models.Notification
// @Failure  404 {object} map[string]string
// @Router   /api/notifications/{id} [patch]
func (h *NotificationsHandler) Patch(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	var req patchNotificationRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.Read == nil {
		return fiber.NewError(fiber.StatusBadRequest, "read is required")
	}
	found, err := h.repo.SetRead(c.Context(), s.UserID, c.Params("id"), *req.Read)
	if err != nil {
		return err
	}
	if !found {
		return fiber.NewError(fiber.StatusNotFound, "notification not found")
	}
	n, err := h.repo.Get(c.Context(), s.UserID, c.Params("id"))
	if err != nil {
		return notFound(err, "notification not found")
	}
	return c.JSON(n)
}

type markAllReadRequest struct {
	// Before bounds the update to notifications the caller has actually seen
	// (seq <= Before), so one that arrives mid-click stays unread. 0 = all.
	Before int64 `json:"before"`
}

// MarkAllRead godoc
// @Summary  Mark all notifications read
// @Tags     notifications
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    body body markAllReadRequest false "optional seq upper bound"
// @Success  200 {object} map[string]int
// @Router   /api/notifications/mark-all-read [post]
func (h *NotificationsHandler) MarkAllRead(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	var req markAllReadRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}
	updated, err := h.repo.MarkAllRead(c.Context(), s.UserID, req.Before)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"updated": updated})
}

// Dismiss godoc
// @Summary  Dismiss (soft-delete) a notification
// @Tags     notifications
// @Security CookieAuth
// @Param    id path string true "Notification id"
// @Success  204
// @Failure  404 {object} map[string]string
// @Router   /api/notifications/{id} [delete]
func (h *NotificationsHandler) Dismiss(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	found, err := h.repo.Dismiss(c.Context(), s.UserID, c.Params("id"))
	if err != nil {
		return err
	}
	if !found {
		return fiber.NewError(fiber.StatusNotFound, "notification not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Stream godoc
// @Summary      Notification stream (SSE)
// @Description  Long-lived Server-Sent Events stream of the caller's
// @Description  notifications. On connect the server replays everything created
// @Description  after the client's cursor (Last-Event-ID header, or ?since=)
// @Description  from the durable log, then switches to live push — so a client
// @Description  that was offline during a burst misses nothing. A fresh client
// @Description  (no cursor) goes live-only and catches history via the REST list.
// @Description
// @Description  Each notification is one frame: `id: <seq>` / `event:
// @Description  notification` / `data: <json>`. A heartbeat comment (`: ping`)
// @Description  is sent every 20 seconds.
// @Tags         notifications
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        Last-Event-ID header string false "Replay cursor (seq)"
// @Param        since         query  int    false "Replay cursor (seq); Last-Event-ID wins"
// @Success      200 "SSE stream"
// @Router       /api/notifications/stream [get]
func (h *NotificationsHandler) Stream(c *fiber.Ctx) error {
	session, err := h.session(c)
	if err != nil {
		return err
	}

	// Replay cursor: Last-Event-ID header wins, else ?since=. Unparseable → no
	// cursor (live-from-now); never 400 a streaming connect.
	cursor := parseCursor(c.Get("Last-Event-Id"), c.Query("since"))

	// Subscribe FIRST, then replay: any notification published between here and
	// the replay query lands in the channel buffer and is deduped by seq during
	// the replay→live handoff, so nothing is dropped across the gap.
	eventCh, unsubscribe, err := h.hub.Subscribe(c.Context(), eventhub.SubscribeOpts{
		UserID:   session.UserID,
		TenantID: session.TenantID,
		Topics:   []string{notify.StreamTopic},
	})
	if err != nil {
		if errors.Is(err, eventhub.ErrTooManySubscribers) {
			return fiber.NewError(fiber.StatusTooManyRequests, err.Error())
		}
		return err
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// Capture before the writer goroutine runs — the fiber ctx is recycled the
	// moment this handler returns (CON-158), so nothing below may touch
	// c.Context(). Build a detached, tenant-scoped ctx for the replay query.
	userID := session.UserID
	sessionID := session.ID
	sessionRepo := h.sessionRepo
	heartbeat := h.heartbeatInterval
	maxLifetime := h.maxLifetime
	repo := h.repo

	reqID, _ := logging.RequestIDFrom(c.Context())
	logCtx := logging.WithRequestID(context.Background(), reqID)
	logCtx = logging.WithUserID(logCtx, session.UserID)
	logCtx = tenantctx.With(logCtx, session.TenantID)
	queryCtx := tenantctx.With(context.Background(), session.TenantID)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		defer unsubscribe()
		// Track *active* connections: increment and decrement in the same
		// goroutine so the gauge can never grow without a matching release
		// (CON-286 — previously it only ever counted up).
		notify.StreamConnections.Add(1)
		defer notify.StreamConnections.Add(-1)

		// Confirm the connection before any real event arrives.
		if err := writeHeartbeat(w); err != nil {
			return
		}

		// Durable replay: everything missed since the cursor, in seq order,
		// before switching to live. Only when a cursor was supplied — a fresh
		// client (no Last-Event-ID) already has history from the REST list.
		var lastSentSeq int64
		if cursor > 0 {
			missed, err := repo.ReplaySince(queryCtx, userID, cursor, maxReplayNotifications)
			if err != nil {
				// Non-fatal: fall through to live so the stream still works.
				slog.ErrorContext(logCtx, "notification replay failed", logging.AttrComponent, "notifications", logging.AttrError, err)
			} else {
				for i := range missed {
					if err := writeNotificationFrame(w, &missed[i]); err != nil {
						return
					}
					lastSentSeq = missed[i].Seq
				}
				notify.StreamReplayed.Add(int64(len(missed)))
			}
		}

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()

		// CON-286: hard lifetime ceiling. Guarantees this goroutine — and the
		// hub slot it holds — is released even if the client vanished without a
		// detectable close and no write ever fails. The client reconnects and
		// replays anything missed via Last-Event-ID (the seq on the id: line).
		lifetime := time.NewTimer(maxLifetime)
		defer lifetime.Stop()

		for {
			select {
			case ev, ok := <-eventCh:
				if !ok {
					// Hub disconnected us (backpressure, eviction, or shutdown).
					return
				}
				n, ok := ev.Payload.(*models.Notification)
				if !ok {
					continue
				}
				// Skip anything already flushed during replay (dedup across the
				// replay→live handoff).
				if n.Seq <= lastSentSeq {
					continue
				}
				if err := writeNotificationFrame(w, n); err != nil {
					slog.ErrorContext(logCtx, "notification sse write failed", logging.AttrComponent, "notifications", logging.AttrError, err)
					return
				}
				lastSentSeq = n.Seq
			case <-ticker.C:
				if err := writeHeartbeat(w); err != nil {
					return
				}
				// Drop the stream if the session was invalidated (logout/expiry)
				// rather than keep delivering to an unauthenticated client.
				if !sessionStillValid(sessionRepo, sessionID) {
					slog.InfoContext(logCtx, "session no longer valid; closing notification stream", logging.AttrComponent, "notifications")
					return
				}
			case <-lifetime.C:
				slog.InfoContext(logCtx, "stream lifetime reached; closing to reclaim slot", logging.AttrComponent, "notifications")
				_ = writeRecycleFrame(w) // best-effort; closing regardless, client reconnects & replays via Last-Event-ID
				return
			}
		}
	}))

	return nil
}

// writeNotificationFrame emits one SSE frame. The `id:` line is the seq — the
// Last-Event-ID cursor a reconnecting client sends back — and `event:
// notification` is the single frame type clients listen for.
func writeNotificationFrame(w *bufio.Writer, n *models.Notification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: notification\ndata: %s\n\n", n.Seq, body); err != nil {
		return err
	}
	return w.Flush()
}

// parseCursor reads the replay cursor: the Last-Event-ID header wins, else the
// ?since= query. A blank or unparseable value means "no cursor" (live-only).
func parseCursor(headerVal, queryVal string) int64 {
	raw := strings.TrimSpace(headerVal)
	if raw == "" {
		raw = strings.TrimSpace(queryVal)
	}
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// seqParam parses a non-negative seq query cursor; empty ⇒ 0 (unset).
func seqParam(c *fiber.Ctx, name string) (int64, error) {
	raw := c.Query(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "invalid "+name+" cursor")
	}
	return v, nil
}

// limitParam parses the page size in [1,100]; empty ⇒ 30. An over-max limit is
// rejected with 400 rather than silently clamped, so the spec and the server
// agree on the page size the caller asked for (CON-242 §9).
func limitParam(c *fiber.Ctx) (int, error) {
	raw := c.Query("limit")
	if raw == "" {
		return 30, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 || v > 100 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	return v, nil
}
