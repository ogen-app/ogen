package telemetry

import (
	"context"

	sentry "github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Propagator is the composite text-map propagator the API installs globally to
// continue and forward distributed traces. Order matters on Extract (the first
// propagator to produce a valid remote parent wins):
//
//   - sentryTracePropagator reads the UI's `sentry-trace` header — the browser
//     SDK emits that, NOT W3C `traceparent`, so without it the UI→API trace is
//     severed (CON-303/304).
//   - TraceContext reads/writes W3C `traceparent`: continues a server-to-server
//     inbound trace and, on Inject, carries this trace to the gRPC microservices
//     and LLM HTTP (which read traceparent).
//   - Baggage carries W3C/Sentry baggage.
//
// Exported so the transport layer installs the exact same propagator in tests
// without re-composing it (and drifting).
func Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		sentryTracePropagator{},
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// sentryTraceHeader is the header the Sentry browser SDK (@sentry/react) sets on
// outbound requests to carry the trace it started. Unlike the Go/OTel side it does
// NOT emit a W3C `traceparent`, so the standard propagation.TraceContext extractor
// never sees it.
const sentryTraceHeader = "sentry-trace"

// sentryTracePropagator continues an inbound Sentry browser trace by reading the
// `sentry-trace` header into an OTel remote span context, which otelfiber then
// adopts as the parent of the server span — joining the UI transaction and the
// API trace under one id in Sentry.
//
// Why this exists: the UI (CON-304) propagates with `sentry-trace`/`baggage`, not
// W3C `traceparent`. sentry-go v0.49 removed its built-in OTel propagator
// (only the error-linking integration remains), so there is otherwise nothing on
// the API that understands the browser's header and the UI→API traces stay
// severed. Extract-only: outbound propagation to the gRPC microservices and LLM
// HTTP stays pure W3C `traceparent` via propagation.TraceContext in the composite,
// which those OTel-instrumented callees already read — so Inject is a no-op here.
//
// The sentry-trace trace id is a 16-byte id, identical in width to an OTel trace
// id and to what the browser reports to Sentry, so adopting it verbatim makes the
// two sides share a trace id and merge in the Sentry waterfall.
type sentryTracePropagator struct{}

var _ propagation.TextMapPropagator = sentryTracePropagator{}

// Extract parses `sentry-trace` and, when valid, returns ctx carrying the browser
// span as a remote parent. A missing or malformed header leaves ctx untouched so
// the next propagator in the composite (W3C traceparent) still gets its chance.
func (sentryTracePropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	header := carrier.Get(sentryTraceHeader)
	if header == "" {
		return ctx
	}
	tpc, ok := sentry.ParseTraceParentContext([]byte(header))
	if !ok {
		return ctx
	}
	// sentry.TraceID/SpanID and their OTel counterparts are the same [16]/[8]byte
	// arrays. Only a positively-sampled parent sets the sampled flag; anything else
	// (SampledFalse/Undefined) stays unsampled and the parent-based sampler drops
	// the trace, matching W3C traceparent behaviour.
	flags := trace.TraceFlags(0)
	if tpc.Sampled == sentry.SampledTrue {
		flags = trace.FlagsSampled
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID(tpc.TraceID),
		SpanID:     trace.SpanID(tpc.ParentSpanID),
		TraceFlags: flags,
		Remote:     true,
	})
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

// Inject is intentionally a no-op: downstream propagation is W3C `traceparent`,
// emitted by propagation.TraceContext alongside this propagator in the composite.
func (sentryTracePropagator) Inject(context.Context, propagation.TextMapCarrier) {}

// Fields reports the header this propagator reads.
func (sentryTracePropagator) Fields() []string { return []string{sentryTraceHeader} }
