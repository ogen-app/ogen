package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/contrib/otelfiber/v2"
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestReqCtxParentsChildSpansUnderServerSpan is the regression guard for CON-303
// DB/gRPC visibility. otelfiber records the server span only on c.UserContext(),
// so a span started from a bare c.Context() — which is what the whole handler layer
// passes into bun/gRPC/otelhttp — is an orphan root that parentlessClientDropSampler
// drops, so DB and gRPC work never appears under the request in Sentry. reqCtx(c)
// must graft the server span onto the request context so those child spans nest
// under it and join the trace.
func TestReqCtxParentsChildSpansUnderServerSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	app := fiber.New()
	// Mirror the production server span source; metrics off like useObservability.
	app.Use(otelfiber.Middleware(otelfiber.WithoutMetrics(true)))
	app.Get("/q", func(c *fiber.Ctx) error {
		// A DB/gRPC-style child started the RIGHT way: from reqCtx(c).
		_, good := otel.Tracer("test").Start(reqCtx(c), "db.query")
		good.End()
		// The OLD way: a bare c.Context() carries no span, so this is an orphan.
		_, orphan := otel.Tracer("test").Start(c.Context(), "db.orphan")
		orphan.End()
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/q", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var server, good, orphan tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch {
		case s.Name == "db.query":
			good = s
		case s.Name == "db.orphan":
			orphan = s
		case s.SpanKind == trace.SpanKindServer:
			server = s
		}
	}
	if !server.SpanContext.IsValid() {
		t.Fatal("otelfiber exported no server span")
	}
	if !good.SpanContext.IsValid() {
		t.Fatal("reqCtx child span (db.query) was not exported")
	}

	// The fix: db.query rides in the server span's trace, parented under it.
	if good.SpanContext.TraceID() != server.SpanContext.TraceID() {
		t.Errorf("reqCtx child trace id = %s, want the server span's %s (not joined)",
			good.SpanContext.TraceID(), server.SpanContext.TraceID())
	}
	if good.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Errorf("reqCtx child parent span id = %s, want the server span's %s (not parented)",
			good.Parent.SpanID(), server.SpanContext.SpanID())
	}

	// The contrast that proves why reqCtx is needed: a bare c.Context() child is an
	// orphan root in its own trace — exactly what used to make DB spans vanish.
	if orphan.SpanContext.IsValid() && orphan.SpanContext.TraceID() == server.SpanContext.TraceID() {
		t.Error("c.Context() child unexpectedly joined the server trace; this test no longer proves the bug")
	}
}
