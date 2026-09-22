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
	return graftRequestSpan(ctx, c)
}

// reqCtx is the context handlers pass into the service/repository layer for the
// SYNCHRONOUS request path. It starts from c.Context() — the *fasthttp.RequestCtx,
// which carries the request's Locals (request/tenant/user/trace ids for log
// correlation, CON-107) and its cancellation — and grafts on the request's
// OpenTelemetry span so the DB/gRPC/HTTP client spans below it nest under the
// server span.
//
// This graft is the missing link for CON-303 DB/gRPC visibility. otelfiber records
// the server span ONLY on c.UserContext(), and the *fasthttp.RequestCtx behind
// c.Context() cannot carry it (its Value reads Locals, not the OTel span key). So a
// bun/gRPC/otelhttp call made with a bare c.Context() starts a span with no parent
// — a parentless client span, which parentlessClientDropSampler drops — which is
// why DB and gRPC work never appeared under the request in Sentry. Passing
// reqCtx(c) makes those client spans children of the server span so they join the
// trace. It is the synchronous-path sibling of detachedContext.
func reqCtx(c *fiber.Ctx) context.Context {
	return graftRequestSpan(c.Context(), c)
}

// graftRequestSpan copies the request's server span context — which otelfiber puts
// only on c.UserContext() — onto ctx, so spans started from ctx become children of
// the server span. The span context is an immutable value (trace id, span id,
// sampled flag), so copying it is safe; children need only the parent's ids, not a
// live parent span. When telemetry is disabled the span context is invalid and ctx
// is returned unchanged.
func graftRequestSpan(ctx context.Context, c *fiber.Ctx) context.Context {
	if sc := trace.SpanContextFromContext(c.UserContext()); sc.IsValid() {
		return trace.ContextWithSpanContext(ctx, sc)
	}
	return ctx
}
