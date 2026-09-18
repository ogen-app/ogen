package server

import (
	"fmt"
	"log/slog"

	sentryfiber "github.com/getsentry/sentry-go/fiber"
	"github.com/gofiber/contrib/otelfiber/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/telemetry"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// useObservability installs the CON-303 tracing + error-capture middleware,
// outermost first, so the server span wraps the whole request and a panic is
// seen by both Sentry and the recover renderer:
//
//  1. otelfiber      — server span, continues an inbound W3C trace, span on
//     c.UserContext(). Metrics off; traces only.
//  2. injectTraceIDs — trace/span ids into Locals for log correlation.
//  3. recover        — renders a 500 and flags the request already-reported.
//  4. sentryfiber    — captures panics at recovery time (stack + trace link) and
//     re-panics so recover (registered outside it) renders the 500.
//
// All effectively no-ops when SENTRY_DSN is unset: no exporter runs and Sentry
// capture has no client, leaving only the cheap local span + recover.
func useObservability(app *fiber.App) {
	app.Use(otelfiber.Middleware(otelfiber.WithoutMetrics(true)))
	app.Use(injectTraceIDs)
	app.Use(recover.New(recover.Config{EnableStackTrace: true, StackTraceHandler: markPanicReported}))
	app.Use(sentryfiber.New(sentryfiber.Options{Repanic: true}))
}

// panicReportedKey marks (in request Locals) that a panic was already captured
// to Sentry by the sentryfiber recovery, so defaultErrorHandler does not report
// the same failure a second time when the recovered 500 flows through it.
type panicReportedKey struct{}

// injectTraceIDs copies the active OpenTelemetry span's trace/span ids (set on
// c.UserContext() by the otelfiber middleware) into the request Locals, so the
// logging ContextHandler attaches them to every line logged with c.Context()
// without any span threading through the handler→repo chain (CON-303). It must
// run immediately after otelfiber.
func injectTraceIDs(c *fiber.Ctx) error {
	if sc := trace.SpanContextFromContext(c.UserContext()); sc.IsValid() {
		c.Locals(logging.TraceIDKey, sc.TraceID().String())
		c.Locals(logging.SpanIDKey, sc.SpanID().String())
	}
	return c.Next()
}

// markPanicReported is the recover middleware's StackTraceHandler. By the time
// it runs, sentryfiber (registered inside recover) has already captured the
// panic to Sentry with its stack and trace link and re-panicked; here we flag
// the request so the error handler skips a duplicate capture, and log it —
// panics bypass the access-log line, so this is the one structured record of it.
func markPanicReported(c *fiber.Ctx, e any) {
	c.Locals(panicReportedKey{}, true)
	slog.ErrorContext(c.Context(), "recovered panic",
		logging.AttrComponent, "http",
		"method", c.Method(),
		"path", c.Path(),
		logging.AttrError, fmt.Errorf("%v", e))
}

// reportServerError sends a 5xx to Sentry, linked to the request's trace and
// tagged with the same correlation ids that appear on its logs. It is a no-op
// when telemetry is disabled, and skips panics (already captured at recovery
// time — see markPanicReported).
func reportServerError(c *fiber.Ctx, err error, status int) {
	if reported, _ := c.Locals(panicReportedKey{}).(bool); reported {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("component", "http"),
		attribute.String("http.method", c.Method()),
		attribute.String("http.route", c.Path()),
		attribute.Int("http.status_code", status),
	}
	if id, ok := logging.RequestIDFrom(c.Context()); ok {
		attrs = append(attrs, attribute.String("request_id", id))
	}
	if id, ok := tenantctx.From(c.Context()); ok {
		attrs = append(attrs, attribute.String("tenant_id", id))
	}
	if id, ok := logging.UserIDFrom(c.Context()); ok {
		attrs = append(attrs, attribute.String("user_id", id))
	}
	// c.UserContext() still carries the otel span + per-request Sentry hub here:
	// the access log invokes the error handler from inside the middleware chain,
	// before otelfiber/sentryfiber restore the context on unwind.
	telemetry.CaptureError(c.UserContext(), err, attrs...)
}
