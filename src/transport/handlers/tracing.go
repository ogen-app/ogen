package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/trace"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// detachedContext builds the context for work that outlives the HTTP handler.
// SSE StreamWriters run after the handler returns and Fiber recycles c, so that
// work cannot borrow c.UserContext() directly — it must start from
// context.Background(). This copies the pieces the detached work still needs:
//
//   - the tenant, so usage recording + entitlement enforcement attribute
//     correctly (CON-86);
//   - the request id, so the detached work's logs stay correlated (CON-107);
//   - the request's OpenTelemetry span context, so the detached flow's spans
//     (Genkit → model → DB) join the SAME trace as the originating request
//     (CON-303), even though the request's server span has already finished by
//     the time the stream runs.
//
// The span context is an immutable value (trace id, span id, sampled flag), so
// capturing it before the handler returns is safe; the parent span object need
// not still be alive when children are created.
func detachedContext(c *fiber.Ctx, tenantID string) context.Context {
	ctx := tenantctx.With(context.Background(), tenantID)
	if reqID, ok := logging.RequestIDFrom(c.Context()); ok {
		ctx = logging.WithRequestID(ctx, reqID)
	}
	if sc := trace.SpanContextFromContext(c.UserContext()); sc.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, sc)
	}
	return ctx
}
