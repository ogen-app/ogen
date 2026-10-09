package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// defaultHeartbeatInterval keeps idle SSE connections alive past
// load-balancer timeouts (typically 30–60s) and gives us a write probe
// to detect dead clients via send failures.
const defaultHeartbeatInterval = 20 * time.Second

// defaultStreamLifetime is the hard ceiling on how long a single SSE
// connection is held open before the writer closes it and reclaims its hub
// subscription. The heartbeat write is not a reliable liveness probe:
// a client that vanishes without a clean close (laptop sleep, NAT/idle timeout,
// a peer advertising a zero window) never surfaces a write error — the tiny
// heartbeats keep buffering into the kernel — so the writer goroutine, and the
// per-user slot it holds, would otherwise leak for the whole process lifetime.
// Bounding the connection's *age* (independent of any write succeeding)
// guarantees the slot is reclaimed; a healthy client simply reconnects, and for
// the notification stream the Last-Event-ID replay covers the gap.
const defaultStreamLifetime = 30 * time.Minute

// setSSEHeaders marks the response as an unbuffered Server-Sent Events stream.
func setSSEHeaders(c *fiber.Ctx) {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")
}

// sseEmit writes one named SSE frame.
type sseEmit func(event string, data any)

// writeNamedEvent emits an `event:` + single-line JSON `data:` frame and
// flushes. Write errors are ignored: the flow runs to completion regardless
// and a vanished client only loses the frames.
func writeNamedEvent(w *bufio.Writer, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	_ = w.Flush()
}

// streamFlow opens an SSE response whose body is produced by run on the
// fasthttp stream-writer goroutine. run executes after the handler returns,
// when the request buffer may already serve another request, so every value
// it captures from the request must be copied (strings.Clone) beforehand.
func streamFlow(c *fiber.Ctx, run func(emit sseEmit)) {
	setSSEHeaders(c)
	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		run(func(event string, data any) { writeNamedEvent(w, event, data) })
	}))
}

// flowErrorPayload is the `error` SSE event body every AI flow emits; its
// JSON shape matches each flow package's ErrorEventPayload.
type flowErrorPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// flowError classifies an AI-flow failure: the flow's validation error V is a
// 400, its model-call error A a 502, anything else a 500 carrying err's text.
func flowError[V, A error](err error) flowErrorPayload {
	if ve, ok := errors.AsType[V](err); ok {
		return flowErrorPayload{Message: ve.Error(), Code: fiber.StatusBadRequest}
	}
	if ae, ok := errors.AsType[A](err); ok {
		return flowErrorPayload{Message: ae.Error(), Code: fiber.StatusBadGateway}
	}
	return flowErrorPayload{Message: err.Error(), Code: fiber.StatusInternalServerError}
}

// hubStream configures a long-lived eventhub-backed SSE connection.
type hubStream struct {
	component   string
	events      <-chan eventhub.Event
	unsubscribe func()
	sessionRepo repository.SessionRepository
	heartbeat   time.Duration
	lifetime    time.Duration

	// writeFailedMsg is logged when write returns an error.
	writeFailedMsg string
	// sessionGoneMsg is logged when the periodic session recheck fails.
	sessionGoneMsg string

	// connected runs first on the writer goroutine; the func it returns runs
	// when the stream closes.
	connected func() (release func())
	// open runs after the initial heartbeat; an error closes the stream
	// silently.
	open func(logCtx context.Context, w *bufio.Writer) error
	// write emits one hub event; an error is logged and closes the stream.
	write func(w *bufio.Writer, ev eventhub.Event) error
}

// streamHub opens an SSE response that forwards hub events until the hub
// closes the channel, a write fails, the session is invalidated, or the
// connection reaches its lifetime ceiling.
//
// The writer goroutine outlives the handler, and by then c.Context() has been
// reset and returned to the fasthttp pool: reading it is a use-after-free that
// nil-panics inside the slog ContextHandler on a goroutine Fiber's recover
// middleware cannot see. Everything the writer needs is captured here, and
// logs go through a detached context carrying request/user/tenant ids.
func streamHub(c *fiber.Ctx, session *models.Session, s hubStream) {
	setSSEHeaders(c)
	sessionID := session.ID
	reqID, _ := logging.RequestIDFrom(reqCtx(c))
	logCtx := logging.WithRequestID(context.Background(), reqID)
	logCtx = logging.WithUserID(logCtx, session.UserID)
	logCtx = tenantctx.With(logCtx, session.TenantID)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		defer s.unsubscribe()
		s.run(logCtx, sessionID, w)
	}))
}

func (s hubStream) run(logCtx context.Context, sessionID string, w *bufio.Writer) {
	if s.connected != nil {
		defer s.connected()()
	}
	// Confirm the connection before any real event arrives.
	if err := writeHeartbeat(w); err != nil {
		return
	}
	if s.open != nil {
		if err := s.open(logCtx, w); err != nil {
			return
		}
	}

	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	// The heartbeat write is not a liveness probe (a vanished client's writes
	// keep buffering in the kernel), so the lifetime ceiling is what
	// guarantees this goroutine and its hub slot are reclaimed.
	lifetime := time.NewTimer(s.lifetime)
	defer lifetime.Stop()

	beats := 0
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				// Hub disconnected us (backpressure, eviction, or shutdown).
				return
			}
			if err := s.write(w, ev); err != nil {
				slog.ErrorContext(logCtx, s.writeFailedMsg, logging.AttrComponent, s.component, logging.AttrError, err)
				return
			}
		case <-ticker.C:
			if err := writeHeartbeat(w); err != nil {
				return
			}
			// Drop the stream once the session is invalidated (logout, expiry,
			// deletion) rather than keep delivering to an unauthenticated client.
			// Checked every few heartbeats, not every one: each open tab holds two
			// streams, and a per-heartbeat lookup is steady DB load for no reader.
			if beats++; beats%sessionRecheckBeats != 0 {
				continue
			}
			if !sessionStillValid(s.sessionRepo, sessionID) {
				slog.InfoContext(logCtx, s.sessionGoneMsg, logging.AttrComponent, s.component)
				return
			}
		case <-lifetime.C:
			slog.InfoContext(logCtx, "stream lifetime reached; closing to reclaim slot", logging.AttrComponent, s.component)
			_ = writeRecycleFrame(w) // best-effort; closing regardless, client reconnects
			return
		}
	}
}

// writeHeartbeat sends a comment frame. SSE comments start with `:` and
// are ignored by clients but keep proxies/load balancers from killing
// the idle connection.
func writeHeartbeat(w *bufio.Writer) error {
	if _, err := w.WriteString(": ping\n\n"); err != nil {
		return err
	}
	return w.Flush()
}

// writeRecycleFrame announces a deliberate server-side connection recycle (the
// lifetime ceiling) just before the stream is closed. A clean close is
// otherwise indistinguishable from a dropped connection, so the client runs its
// full reconnect recovery (cache reconcile / "catching up" UI) on every 30-min
// recycle even though nothing was down. This lets a client that opts in
// (addEventListener('recycle', …)) skip that catch-up honestly. No `id:` line —
// this is not a replayable event and must not advance the Last-Event-ID cursor.
func writeRecycleFrame(w *bufio.Writer) error {
	if _, err := w.WriteString("event: recycle\ndata: {\"reason\":\"lifetime\"}\n\n"); err != nil {
		return err
	}
	return w.Flush()
}

const (
	// sessionRecheckBeats is how many heartbeats pass between session
	// rechecks: two minutes at the default 20 s heartbeat.
	sessionRecheckBeats = 6
	// sessionRecheckTimeout bounds one recheck so a stalled database cannot
	// stall the stream goroutine.
	sessionRecheckTimeout = 5 * time.Second
)

// sessionStillValid runs a fresh repo lookup. Background context — the
// fiber request ctx is gone by the time the stream writer runs.
func sessionStillValid(repo repository.SessionRepository, id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), sessionRecheckTimeout)
	defer cancel()
	s, err := repo.GetByID(ctx, id)
	if err != nil || s == nil {
		return false
	}
	return time.Now().UTC().Before(s.ExpiresAt)
}
