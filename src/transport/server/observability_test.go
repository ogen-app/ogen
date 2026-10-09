package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/telemetry"
)

// newObservabilityTestApp builds a Fiber app with the production observability
// chain (useObservability) and the real defaultErrorHandler, plus an in-memory
// span exporter installed as the global OTel provider so otelfiber produces
// real spans, and the production propagator so it continues inbound traces.
func newObservabilityTestApp(t *testing.T) (*fiber.App, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(telemetry.Propagator())
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	app := fiber.New(fiber.Config{ErrorHandler: defaultErrorHandler})
	useObservability(app)
	return app, exp
}

// TestObservabilityPanicBecomes500 is the regression guard for the subtle
// recover/sentryScope ordering: a handler panic must be turned into a clean 500
// (never a 200 or a process crash), even with Sentry disabled.
func TestObservabilityPanicBecomes500(t *testing.T) {
	app, _ := newObservabilityTestApp(t)
	app.Get("/panic", func(*fiber.Ctx) error { panic("boom") })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/panic", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("panic route: got status %d, want 500", resp.StatusCode)
	}
}

// TestObservabilityErrorBecomes500 checks a normally-returned 5xx still renders
// (the added Sentry capture is a no-op without a DSN and must not disrupt it).
func TestObservabilityErrorBecomes500(t *testing.T) {
	app, _ := newObservabilityTestApp(t)
	app.Get("/fail", func(*fiber.Ctx) error {
		return fiber.NewError(fiber.StatusInternalServerError, "kaboom")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("fail route: got status %d, want 500", resp.StatusCode)
	}
}

// TestObservabilityInjectsTraceIDs verifies otelfiber + injectTraceIDs make the
// active trace id available to handlers via c.Context() (the log-correlation
// seam), and that a span is exported for the request.
func TestObservabilityInjectsTraceIDs(t *testing.T) {
	app, exp := newObservabilityTestApp(t)
	app.Get("/ok", func(c *fiber.Ctx) error {
		id, ok := logging.TraceIDFrom(c.Context())
		if !ok || id == "" {
			t.Error("trace id not present in request context Locals")
		}
		c.Set("X-Test-Trace", id)
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ok", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("ok route: got status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Test-Trace") == "" {
		t.Error("handler saw no trace id")
	}
	if len(exp.GetSpans()) == 0 {
		t.Error("otelfiber exported no span for the request")
	}
}

// TestObservabilityContinuesSentryBrowserTrace is the regression guard for the
// UI→API disconnect (CON-303/304): the Sentry browser SDK propagates its trace
// with a `sentry-trace` header (not W3C `traceparent`). The server span must
// adopt the browser's trace id so the two join into one Sentry waterfall, rather
// than otelfiber starting a fresh root trace per request.
func TestObservabilityContinuesSentryBrowserTrace(t *testing.T) {
	app, exp := newObservabilityTestApp(t)
	app.Get("/ok", func(c *fiber.Ctx) error { return c.SendString("ok") })

	const browserTraceID = "d49d9bf66f13450b81f65bc51cf49c03"
	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("sentry-trace", browserTraceID+"-7c51fd1f2147d9d5-1")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("ok route: got status %d, want 200", resp.StatusCode)
	}

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("otelfiber exported no span for the request")
	}
	if got := spans[0].SpanContext.TraceID().String(); got != browserTraceID {
		t.Errorf("server span trace id = %s, want the browser's %s (trace not continued)", got, browserTraceID)
	}
	if !spans[0].Parent.IsRemote() {
		t.Error("server span parent should be the remote browser span")
	}
}

// TestSentryScopeCapturesPanicWithoutBody checks that, with Sentry enabled, a
// panic is reported once with the request's method, URL and safe headers, and
// that the request body never reaches the event.
func TestSentryScopeCapturesPanicWithoutBody(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Dsn: "https://key@sentry.invalid/1", Transport: transport})
	if err != nil {
		t.Fatalf("sentry client: %v", err)
	}
	hub := sentry.CurrentHub()
	prev := hub.Client()
	hub.BindClient(client)
	t.Cleanup(func() { hub.BindClient(prev) })

	app, _ := newObservabilityTestApp(t)
	app.Post("/panic", func(*fiber.Ctx) error { panic("boom") })

	req := httptest.NewRequest(http.MethodPost, "/panic", strings.NewReader(`{"secret":"body"}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set(fiber.HeaderUserAgent, "probe/1.0")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("panic route: got status %d, want 500", resp.StatusCode)
	}

	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured %d events, want 1", len(events))
	}
	r := events[0].Request
	if r == nil {
		t.Fatal("event has no request")
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL, "/panic") {
		t.Errorf("request = %s %s, want POST …/panic", r.Method, r.URL)
	}
	if r.Headers[fiber.HeaderUserAgent] != "probe/1.0" {
		t.Errorf("user agent = %q, want probe/1.0", r.Headers[fiber.HeaderUserAgent])
	}
	if r.Data != "" {
		t.Errorf("request body leaked into the event: %q", r.Data)
	}
}
