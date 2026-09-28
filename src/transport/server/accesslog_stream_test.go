package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// TestAccessLogDoesNotDrainStreamingResponse guards a regression in accessLog.
//
// accessLog runs after the handler and previously recorded the response size
// with len(c.Response().Body()). For a response produced by SetBodyStreamWriter
// (the SSE event stream), fasthttp's Body() drains the stream to EOF to buffer
// it. A live SSE stream never ends, so that call blocked inside the middleware —
// before fasthttp ever wrote the response — and the client hung indefinitely
// (observed as an event stream stuck in "connecting").
//
// The test serves a streaming endpoint on a real socket and asserts the first
// frame arrives while the stream is still open. With the regression, nothing is
// ever flushed and the read below hits its deadline.
func TestAccessLogDoesNotDrainStreamingResponse(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(accessLog())

	released := make(chan struct{})
	app.Get("/stream", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/event-stream")
		c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
			_, _ = w.WriteString(": ping\n\n") // initial heartbeat, like the events handler
			_ = w.Flush()
			<-released // hold the connection open like a live SSE stream
		})
		return nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() {
		close(released)
		_ = app.ShutdownWithTimeout(3 * time.Second)
	})

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := fmt.Fprint(conn, "GET /stream HTTP/1.1\r\nHost: test\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// The heartbeat must reach us while the stream is still open (before we
	// close `released`). The regression blocks accessLog on Body(), so nothing
	// is flushed and this read hits the deadline instead.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("expected streamed heartbeat before the stream closed, got read error "+
				"(regression: accessLog drained/blocked the SSE stream): %v", err)
		}
		if strings.HasPrefix(line, ": ping") {
			return // received the streamed frame while the stream is still open — success
		}
	}
}
