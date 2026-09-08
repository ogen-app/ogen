package handlers_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// These tests guard CON-158: the SSE stream writer runs on a fasthttp
// goroutine *after* the Fiber handler returns, once the *fasthttp.RequestCtx
// has been reset and pooled. Reading c.Context() from the writer is then a
// use-after-free that nil-panics inside the slog ContextHandler, on a
// goroutine the Fiber recover middleware can't see — so the whole process
// dies. The fix detaches a logging context before SetBodyStreamWriter; these
// tests drive a real socket so the recycle/disconnect ordering is exercised
// (app.Test's in-memory recorder can't reproduce it).

// stubSessionRepo satisfies repository.SessionRepository without a database.
// GetByID returns (nil, nil), which sessionStillValid treats as "invalid" —
// used by the session-recheck test to drive the "closing stream" branch.
type stubSessionRepo struct{}

func (stubSessionRepo) Create(context.Context, *models.Session) error { return nil }
func (stubSessionRepo) CreateTx(context.Context, bun.IDB, *models.Session) error {
	return nil
}
func (stubSessionRepo) GetByID(context.Context, string) (*models.Session, error) { return nil, nil }
func (stubSessionRepo) SetDefaultWorkspace(context.Context, string, string, string) error {
	return nil
}
func (stubSessionRepo) Delete(context.Context, string) (bool, error) { return false, nil }

// captureHandler records the slog.Records it receives so a test can assert on
// the correlation attributes the ContextHandler attached from the log call's
// context. Safe for the concurrent writes coming off the stream goroutine.
type captureHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone())
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// installCaptureLogger points slog's default at a capturing handler wrapped in
// the real logging.ContextHandler (same decoration production uses), so the
// request_id / tenant_id / user_id attrs get attached exactly as they would in
// the running server. The previous default is restored on cleanup.
func installCaptureLogger(t *testing.T) (*sync.Mutex, *[]slog.Record) {
	t.Helper()
	var (
		mu      sync.Mutex
		records []slog.Record
	)
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.ContextHandler{Handler: captureHandler{mu: &mu, records: &records}}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &mu, &records
}

// findRecord returns the first captured record whose message matches, or false.
func findRecord(mu *sync.Mutex, records *[]slog.Record, msg string) (slog.Record, bool) {
	mu.Lock()
	defer mu.Unlock()
	for _, r := range *records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// attrString extracts a string attribute value from a record.
func attrString(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out, found = a.Value.String(), true
			return false
		}
		return true
	})
	return out, found
}

// waitFor polls cond until it holds or the deadline elapses.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// serveEventsOnSocket wires an EventsHandler behind a fake auth middleware that
// injects the session + request id, then serves it on a real loopback socket.
func serveEventsOnSocket(t *testing.T, hub eventhub.Hub, heartbeat time.Duration) (addr string) {
	t.Helper()
	return serveEventsOnSocketWithLifetime(t, hub, heartbeat, 0)
}

// serveEventsOnSocketWithLifetime is serveEventsOnSocket plus a per-connection
// lifetime override (0 = handler default). Used by the CON-286 lifetime-cap test.
func serveEventsOnSocketWithLifetime(t *testing.T, hub eventhub.Hub, heartbeat, lifetime time.Duration) (addr string) {
	t.Helper()
	fakeAuth := func(c *fiber.Ctx) error {
		c.Locals("session", &models.Session{ID: "sess-1", UserID: "user-1", TenantID: "tenant-1"})
		c.Locals(logging.RequestIDKey, "req-1")
		return c.Next()
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	h := handlers.NewEventsHandler(hub, stubSessionRepo{}, fakeAuth, heartbeat)
	h.SetMaxLifetime(lifetime)
	h.Register(app)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.ShutdownWithTimeout(3 * time.Second) })
	return ln.Addr().String()
}

// readInitialHeartbeat consumes the status line, headers and the initial SSE
// heartbeat frame, confirming the stream writer goroutine is live and parked.
func readInitialHeartbeat(t *testing.T, br *bufio.Reader) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("expected initial heartbeat before any error: %v", err)
		}
		if strings.Contains(line, "ping") {
			return
		}
	}
}

// TestStreamSurvivesClientDisconnectMidWrite is the core CON-158 regression:
// a client drops mid-stream, the hub then pushes an event, the writer's flush
// fails, and the error is logged. The pre-fix code logged with the recycled
// c.Context() and nil-panicked the process; here the process must stay up, the
// subscription must be released, and the failure log must still correlate.
func TestStreamSurvivesClientDisconnectMidWrite(t *testing.T) {
	mu, records := installCaptureLogger(t)

	// Buffer >= 1 so Publish never blocks after the client is gone.
	hub := newStubHub(8)
	addr := serveEventsOnSocket(t, hub, time.Hour) // big heartbeat: isolate the write-failure path

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := fmt.Fprint(conn, "GET /api/events?topics=all HTTP/1.1\r\nHost: test\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	readInitialHeartbeat(t, bufio.NewReader(conn))

	// Drop the client mid-stream, then keep pushing events until the writer's
	// flush fails and it tears down the subscription. A single write after the
	// client vanishes can still be buffered successfully — fasthttp only
	// surfaces the dead connection to the stream writer on a subsequent flush,
	// so we drive events the way the live event/heartbeat loop would. Sends are
	// non-blocking so a full buffer (writer already gone) can't wedge us.
	_ = conn.Close()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				select {
				case hub.ch <- eventhub.Event{ID: "ev", Topic: "all", Type: "updated"}:
				default:
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer close(done)

	// If we reach here without the test binary crashing, no panic escaped the
	// writer goroutine. The writer must also have released the subscription.
	waitFor(t, 3*time.Second, "stream writer to unsubscribe after client disconnect", func() bool {
		return hub.unsubbed.Load() == 1
	})

	// AC: the write-failure log survives with request_id / tenant_id / user_id.
	waitFor(t, 3*time.Second, `"sse write failed" log record`, func() bool {
		_, ok := findRecord(mu, records, "sse write failed")
		return ok
	})
	rec, _ := findRecord(mu, records, "sse write failed")
	assertCorrelated(t, rec)
}

// TestStreamSessionRecheckUsesDetachedContext covers the second latent site:
// the ticker branch that closes the stream when the session is no longer
// valid. With a fast heartbeat and a repo that reports the session gone, the
// writer logs "session no longer valid; closing stream" and returns — and that
// log must use the detached context too (no c.Context() read, correlated).
func TestStreamSessionRecheckUsesDetachedContext(t *testing.T) {
	mu, records := installCaptureLogger(t)

	hub := newStubHub(1)
	addr := serveEventsOnSocket(t, hub, 20*time.Millisecond) // fast heartbeat drives the recheck

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := fmt.Fprint(conn, "GET /api/events?topics=all HTTP/1.1\r\nHost: test\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// Keep reading so heartbeat writes succeed until the server closes the
	// stream itself (the session-invalid path), which surfaces here as EOF.
	go func() {
		br := bufio.NewReader(conn)
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })

	waitFor(t, 3*time.Second, `"session no longer valid" log record`, func() bool {
		_, ok := findRecord(mu, records, "session no longer valid; closing stream")
		return ok
	})
	waitFor(t, 3*time.Second, "stream writer to unsubscribe after session invalidation", func() bool {
		return hub.unsubbed.Load() == 1
	})
	rec, _ := findRecord(mu, records, "session no longer valid; closing stream")
	assertCorrelated(t, rec)
}

// TestStreamClosesAtLifetimeCap is the CON-286 regression: a connection that
// never errors on write (the leak's root cause — a vanished peer whose tiny
// heartbeats keep buffering) must still be reclaimed. With a short lifetime cap
// and a big heartbeat (so the write/session paths can't be what closes it), the
// writer must hit the lifetime branch, log it, and release the subscription.
func TestStreamClosesAtLifetimeCap(t *testing.T) {
	mu, records := installCaptureLogger(t)

	hub := newStubHub(1)
	// Big heartbeat isolates the lifetime path; short lifetime drives it fast.
	addr := serveEventsOnSocketWithLifetime(t, hub, time.Hour, 60*time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := fmt.Fprint(conn, "GET /api/events?topics=all HTTP/1.1\r\nHost: test\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// Keep the client alive and reading so nothing but the lifetime cap can end
	// the stream; the server closes it, which surfaces here as EOF.
	go func() {
		br := bufio.NewReader(conn)
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })

	waitFor(t, 3*time.Second, `"stream lifetime reached" log record`, func() bool {
		_, ok := findRecord(mu, records, "stream lifetime reached; closing to reclaim slot")
		return ok
	})
	waitFor(t, 3*time.Second, "stream writer to unsubscribe after lifetime cap", func() bool {
		return hub.unsubbed.Load() == 1
	})
	rec, _ := findRecord(mu, records, "stream lifetime reached; closing to reclaim slot")
	assertCorrelated(t, rec)
}

// assertCorrelated checks the CON-158 acceptance criterion that stream-writer
// logs still carry the request/tenant/user correlation attributes.
func assertCorrelated(t *testing.T, rec slog.Record) {
	t.Helper()
	for _, want := range []struct{ key, val string }{
		{logging.AttrRequestID, "req-1"},
		{logging.AttrTenantID, "tenant-1"},
		{logging.AttrUserID, "user-1"},
	} {
		got, ok := attrString(rec, want.key)
		if !ok {
			t.Errorf("log record %q missing %s attribute", rec.Message, want.key)
			continue
		}
		if got != want.val {
			t.Errorf("log record %q: %s = %q, want %q", rec.Message, want.key, got, want.val)
		}
	}
}
