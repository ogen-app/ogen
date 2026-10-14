package jobs

import (
	"context"
	"errors"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/telemetry"
)

// Middleware returns the River worker middleware Ogen installs on its job
// client, outermost first: tracing wraps the drain handling, so a job
// rescheduled by a shutdown shows as a snooze in its span, not a failure.
func Middleware() []rivertype.Middleware {
	return []rivertype.Middleware{newTracingMiddleware(), &drainMiddleware{}}
}

// tracingMiddleware wraps every River job in a root OpenTelemetry span named by
// job kind. Background jobs run with no inbound trace, so their DB / gRPC / HTTP
// work would otherwise be parentless client spans the telemetry sampler drops;
// giving each job its own root span makes that work a coherent trace and
// correlates the job's logs via trace_id/span_id. Failures are reported to
// Sentry only once retries are exhausted, so a transient error that later
// succeeds doesn't spam. A no-op when telemetry is disabled (noop global tracer).
type tracingMiddleware struct {
	river.MiddlewareDefaults
	tracer trace.Tracer
}

func newTracingMiddleware() *tracingMiddleware {
	return &tracingMiddleware{tracer: otel.Tracer("ogen/jobs")}
}

func (m *tracingMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	ctx, span := m.tracer.Start(ctx, "job "+job.Kind,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("job.kind", job.Kind),
			attribute.String("job.queue", job.Queue),
			attribute.Int("job.attempt", job.Attempt),
			attribute.Int64("job.id", job.ID),
		))
	defer span.End()

	// Correlate the job's logs with its trace, mirroring the request path.
	if sc := span.SpanContext(); sc.IsValid() {
		ctx = context.WithValue(ctx, logging.TraceIDKey, sc.TraceID().String())
		ctx = context.WithValue(ctx, logging.SpanIDKey, sc.SpanID().String())
	}

	err := doInner(ctx)
	// A snooze reschedules the job without using an attempt; it is not a
	// failure.
	if _, snoozed := errors.AsType[*rivertype.JobSnoozeError](err); snoozed {
		span.SetAttributes(attribute.Bool("job.snoozed", true))
		return err
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		// Report only when River won't retry again, so transient-then-recovered
		// failures don't create a Sentry issue per attempt.
		if job.Attempt >= job.MaxAttempts {
			telemetry.CaptureError(ctx, err,
				attribute.String("component", "jobs"),
				attribute.String("job.kind", job.Kind),
				attribute.Int("job.attempt", job.Attempt))
		}
	}
	return err
}
