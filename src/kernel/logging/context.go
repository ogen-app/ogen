// Package logging is Ogen's single structured-logging foundation (CON-107).
// It builds a configured *slog.Logger (JSON or text, level-controlled),
// installs it as slog's default, and enriches every record with the
// request/tenant/user ids carried by the log call's context.
//
// Correlation piggybacks on the same mechanism tenantctx uses on the request
// path: Fiber's c.Locals(key, value) stores into the fasthttp RequestCtx, which
// exposes values through (*RequestCtx).Value, so a value set via
// c.Locals(logging.RequestIDKey, id) reads back through ctx.Value on the
// request context. Call sites therefore only need to pass c.Context() into
// slog.*Context — no c.UserContext() threading through the handler→repo chain.
package logging

import (
	"context"
	"log/slog"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Attribute keys used across the codebase. Centralised so the field names stay
// consistent and greppable.
const (
	AttrComponent = "component"
	AttrRequestID = "request_id"
	AttrTenantID  = "tenant_id"
	AttrUserID    = "user_id"
	AttrError     = "err"
	// AttrTraceID / AttrSpanID correlate a log line with its distributed trace
	// (CON-303). The values are the active OpenTelemetry span's ids, injected
	// into the request Locals by the tracing middleware so — like the ids above —
	// call sites keep passing c.Context() with no span threading.
	AttrTraceID = "trace_id"
	AttrSpanID  = "span_id"
)

// ctxKey types are unexported so the correlation keys cannot collide with — or
// be forged by — other packages, mirroring tenantctx.Key. Their exported *Key
// values double as Fiber c.Locals keys (see the package doc).
type requestIDKey struct{}
type userIDKey struct{}
type traceIDKey struct{}
type spanIDKey struct{}

// RequestIDKey and UserIDKey are the context (and Fiber Locals) keys under
// which the request id and user id are stored. Exported as values of
// unexported types so callers can set/read them without being able to forge the
// key type. TraceIDKey / SpanIDKey hold the active trace/span ids (CON-303).
var (
	RequestIDKey = requestIDKey{}
	UserIDKey    = userIDKey{}
	TraceIDKey   = traceIDKey{}
	SpanIDKey    = spanIDKey{}
)

// WithRequestID returns a copy of ctx carrying the given request id. Used on
// the job path, where there is no Fiber context to inherit from.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, RequestIDKey, id)
}

// WithUserID returns a copy of ctx carrying the given user id.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, UserIDKey, id)
}

// RequestIDFrom returns the request id carried by ctx and whether a non-empty
// one was present.
func RequestIDFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(RequestIDKey).(string)
	return v, ok && v != ""
}

// UserIDFrom returns the user id carried by ctx and whether a non-empty one was
// present.
func UserIDFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(UserIDKey).(string)
	return v, ok && v != ""
}

// TraceIDFrom returns the trace id carried by ctx and whether a non-empty one
// was present.
func TraceIDFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(TraceIDKey).(string)
	return v, ok && v != ""
}

// SpanIDFrom returns the span id carried by ctx and whether a non-empty one was
// present.
func SpanIDFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(SpanIDKey).(string)
	return v, ok && v != ""
}

// ContextHandler decorates a slog.Handler, enriching every record with the
// request id, tenant id, and user id carried by the log call's context. Values
// absent from the context are omitted — no empty attributes are emitted, so
// startup logs (before any middleware runs) and system-context jobs stay clean.
type ContextHandler struct {
	slog.Handler
}

// Handle adds the correlation attributes (when present) and forwards to the
// wrapped handler.
func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if id, ok := RequestIDFrom(ctx); ok {
			r.AddAttrs(slog.String(AttrRequestID, id))
		}
		if id, ok := tenantctx.From(ctx); ok {
			r.AddAttrs(slog.String(AttrTenantID, id))
		}
		if id, ok := UserIDFrom(ctx); ok {
			r.AddAttrs(slog.String(AttrUserID, id))
		}
		if id, ok := TraceIDFrom(ctx); ok {
			r.AddAttrs(slog.String(AttrTraceID, id))
		}
		if id, ok := SpanIDFrom(ctx); ok {
			r.AddAttrs(slog.String(AttrSpanID, id))
		}
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs re-wraps so the decorator survives logger.With(...) chains.
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup re-wraps so the decorator survives logger.WithGroup(...) chains.
func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithGroup(name)}
}
