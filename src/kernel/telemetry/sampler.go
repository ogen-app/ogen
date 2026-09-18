package telemetry

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// parentlessClientDropSampler drops spans that are BOTH client-kind AND have no
// parent, deferring every other decision to base.
//
// bun's query hook (and the gRPC client handler) start a client span for every
// call. Within a request or job those are children of the server/job span and
// ride its sampling decision — exactly what we want. But a client call made
// outside any trace (boot migrations, background analytics writes, a query in a
// job that hasn't opened its own span yet) would otherwise become a standalone
// root span and, at the head-sample ratio, a noisy one-span "trace" in Sentry.
// Dropping the parentless client case keeps the DB/gRPC layer visible only where
// it belongs — under a real request or job trace.
//
// It intentionally does NOT drop parentless server spans (otelfiber's inbound
// span is a parentless server span and must be sampled) or internal spans
// (Genkit flow spans).
type parentlessClientDropSampler struct{ base sdktrace.Sampler }

func (s parentlessClientDropSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if p.Kind == trace.SpanKindClient && !trace.SpanContextFromContext(p.ParentContext).IsValid() {
		return sdktrace.SamplingResult{Decision: sdktrace.Drop}
	}
	return s.base.ShouldSample(p)
}

func (s parentlessClientDropSampler) Description() string {
	return "ParentlessClientDrop{" + s.base.Description() + "}"
}
