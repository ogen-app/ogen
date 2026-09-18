package telemetry

import (
	"context"
	"errors"
	"os"
	"testing"

	sentry "github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/ogen-app/ogen/src/kernel/config"
)

// TestInitDisabledIsNoop verifies the fail-open contract: an empty DSN leaves
// Sentry uninitialised and returns a usable no-op shutdown, so callers can wire
// telemetry unconditionally and local/dev runs are untouched.
func TestInitDisabledIsNoop(t *testing.T) {
	shutdown, err := Init(context.Background(), &config.Config{SentryDSN: ""})
	if err != nil {
		t.Fatalf("Init with empty DSN returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	// CaptureError must be a safe no-op when telemetry is disabled.
	CaptureError(context.Background(), errors.New("should be dropped"))
}

// TestScrubEventStripsSensitive verifies D3: bodies, cookies, query strings and
// auth headers never leave the process on an error event.
func TestScrubEventStripsSensitive(t *testing.T) {
	ev := &sentry.Event{
		Request: &sentry.Request{
			URL:         "https://api.example.com/api/posts",
			QueryString: "token=secret",
			Cookies:     "c3_session=abc",
			Data:        `{"content":"private draft"}`,
			Headers: map[string]string{
				"Authorization": "Bearer x",
				"Cookie":        "c3_session=abc",
				"X-Admin-Token": "super-secret",
				"Content-Type":  "application/json",
			},
		},
	}
	got := scrubEvent(ev, nil)
	if got.Request.QueryString != "" || got.Request.Cookies != "" || got.Request.Data != "" {
		t.Errorf("query/cookies/data not scrubbed: %+v", got.Request)
	}
	if _, ok := got.Request.Headers["Authorization"]; ok {
		t.Error("Authorization header not scrubbed")
	}
	if _, ok := got.Request.Headers["Cookie"]; ok {
		t.Error("Cookie header not scrubbed")
	}
	if _, ok := got.Request.Headers["X-Admin-Token"]; ok {
		t.Error("X-Admin-Token header not scrubbed")
	}
	if got.Request.Headers["Content-Type"] != "application/json" {
		t.Error("non-sensitive header should be preserved")
	}
	if got.Request.URL != "https://api.example.com/api/posts" {
		t.Error("URL path should be preserved")
	}
}

// TestLiveSmoke exercises the real pipeline end-to-end against a Sentry project.
// It is skipped unless OGEN_SENTRY_SMOKE_DSN is set, so it never runs in CI. Run
// it manually to confirm an error + a span reach Sentry:
//
//	OGEN_SENTRY_SMOKE_DSN="https://…@…ingest…/…" go test ./src/kernel/telemetry/ -run TestLiveSmoke -v
func TestLiveSmoke(t *testing.T) {
	dsn := os.Getenv("OGEN_SENTRY_SMOKE_DSN")
	if dsn == "" {
		t.Skip("set OGEN_SENTRY_SMOKE_DSN to run the live Sentry smoke test")
	}
	ctx := context.Background()
	shutdown, err := Init(ctx, &config.Config{
		SentryDSN:         dsn,
		SentryEnvironment: "smoke-test",
		SentryRelease:     "con-303-smoke",
		SentrySampleRate:  1.0, // export every span
		SentryDebug:       true,
		OTelServiceName:   "ogen-api-smoke",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// A root span, an error captured inside it (should link to the trace), and
	// span end — then flush.
	spanCtx, span := otel.Tracer("con-303-smoke").Start(ctx, "smoke-root-span")
	CaptureError(spanCtx, errors.New("CON-303 telemetry smoke test error"),
		attribute.String("component", "smoke"),
		attribute.Int("attempt", 1),
	)
	span.End()

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown/flush: %v", err)
	}
}
