package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestParentlessClientDropSampler(t *testing.T) {
	// base always samples, so any non-dropped decision is RecordAndSample.
	s := parentlessClientDropSampler{base: sdktrace.AlwaysSample()}

	validParent := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x1},
		SpanID:     trace.SpanID{0x2},
		TraceFlags: trace.FlagsSampled,
	}))

	cases := []struct {
		name   string
		params sdktrace.SamplingParameters
		want   sdktrace.SamplingDecision
	}{
		{
			name:   "parentless client span is dropped",
			params: sdktrace.SamplingParameters{ParentContext: context.Background(), Kind: trace.SpanKindClient},
			want:   sdktrace.Drop,
		},
		{
			name:   "parentless server span is kept (otelfiber inbound)",
			params: sdktrace.SamplingParameters{ParentContext: context.Background(), Kind: trace.SpanKindServer},
			want:   sdktrace.RecordAndSample,
		},
		{
			name:   "parentless internal span is kept (job/flow root)",
			params: sdktrace.SamplingParameters{ParentContext: context.Background(), Kind: trace.SpanKindInternal},
			want:   sdktrace.RecordAndSample,
		},
		{
			name:   "child client span is kept (DB call under a request)",
			params: sdktrace.SamplingParameters{ParentContext: validParent, Kind: trace.SpanKindClient},
			want:   sdktrace.RecordAndSample,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.ShouldSample(tc.params).Decision; got != tc.want {
				t.Fatalf("decision = %v, want %v", got, tc.want)
			}
		})
	}
}
