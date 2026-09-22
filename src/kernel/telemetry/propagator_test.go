package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// A well-formed sentry-trace header: <32-hex trace id>-<16-hex span id>-<sampled>.
const (
	sampleTraceID = "d49d9bf66f13450b81f65bc51cf49c03"
	sampleSpanID  = "7c51fd1f2147d9d5"
)

func TestSentryTracePropagatorExtract(t *testing.T) {
	p := sentryTracePropagator{}

	t.Run("sampled header becomes a sampled remote parent", func(t *testing.T) {
		carrier := propagation.MapCarrier{sentryTraceHeader: sampleTraceID + "-" + sampleSpanID + "-1"}

		sc := trace.SpanContextFromContext(p.Extract(context.Background(), carrier))
		if !sc.IsValid() {
			t.Fatal("expected a valid remote span context")
		}
		if got := sc.TraceID().String(); got != sampleTraceID {
			t.Errorf("trace id = %s, want %s", got, sampleTraceID)
		}
		if got := sc.SpanID().String(); got != sampleSpanID {
			t.Errorf("span id = %s, want %s", got, sampleSpanID)
		}
		if !sc.IsSampled() {
			t.Error("expected the parent to be sampled")
		}
		if !sc.IsRemote() {
			t.Error("expected the parent to be marked remote")
		}
	})

	t.Run("unsampled header carries the id but not the sampled flag", func(t *testing.T) {
		carrier := propagation.MapCarrier{sentryTraceHeader: sampleTraceID + "-" + sampleSpanID + "-0"}

		sc := trace.SpanContextFromContext(p.Extract(context.Background(), carrier))
		if !sc.IsValid() {
			t.Fatal("expected a valid remote span context")
		}
		if sc.IsSampled() {
			t.Error("expected the parent to be unsampled")
		}
	})

	t.Run("missing header leaves the context untouched", func(t *testing.T) {
		sc := trace.SpanContextFromContext(p.Extract(context.Background(), propagation.MapCarrier{}))
		if sc.IsValid() {
			t.Error("expected no span context for a missing header")
		}
	})

	t.Run("malformed header leaves the context untouched", func(t *testing.T) {
		carrier := propagation.MapCarrier{sentryTraceHeader: "not-a-sentry-trace"}
		sc := trace.SpanContextFromContext(p.Extract(context.Background(), carrier))
		if sc.IsValid() {
			t.Error("expected no span context for a malformed header")
		}
	})
}

// Inject must not write anything: downstream propagation is W3C traceparent via
// the TraceContext propagator, and this one is extract-only.
func TestSentryTracePropagatorInjectIsNoop(t *testing.T) {
	carrier := propagation.MapCarrier{}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x1},
		SpanID:     trace.SpanID{0x2},
		TraceFlags: trace.FlagsSampled,
	}))

	sentryTracePropagator{}.Inject(ctx, carrier)
	if len(carrier) != 0 {
		t.Errorf("Inject wrote %d keys, want 0", len(carrier))
	}
}

func TestSentryTracePropagatorFields(t *testing.T) {
	got := sentryTracePropagator{}.Fields()
	if len(got) != 1 || got[0] != sentryTraceHeader {
		t.Errorf("Fields() = %v, want [%s]", got, sentryTraceHeader)
	}
}

// When a request carries BOTH a valid `sentry-trace` and a valid W3C
// `traceparent`, the composite must keep the browser's Sentry trace as the
// parent — it is the intended head of the end-to-end trace. This is what pins
// sentryTracePropagator after TraceContext in Propagator() (last valid extractor
// wins), so guard the order here.
func TestPropagatorPrefersSentryTraceOverTraceparent(t *testing.T) {
	const traceparentTraceID = "11111111111111111111111111111111"
	carrier := propagation.MapCarrier{
		sentryTraceHeader: sampleTraceID + "-" + sampleSpanID + "-1",
		"traceparent":     "00-" + traceparentTraceID + "-2222222222222222-01",
	}

	sc := trace.SpanContextFromContext(Propagator().Extract(context.Background(), carrier))
	if !sc.IsValid() {
		t.Fatal("expected a valid remote span context")
	}
	if got := sc.TraceID().String(); got != sampleTraceID {
		t.Errorf("trace id = %s, want the sentry-trace id %s (traceparent must not win)", got, sampleTraceID)
	}
}
