// Package telemetry wires error monitoring and OpenTelemetry tracing, unified in
// Sentry (CON-303). Instrumentation across the codebase is vendor-neutral
// OpenTelemetry; this package owns the single place that decides where those
// signals go:
//
//   - Traces: a global *sdktrace.TracerProvider whose batch exporter ships OTel
//     spans to Sentry's OTLP endpoint (endpoint + auth derived from the DSN by
//     sentryotlp). Because it is a concrete *sdktrace.TracerProvider set as the
//     OTel global, Genkit reuses it (firebase/genkit/go/core/tracing resolves the
//     global provider when it is already an *sdktrace.TracerProvider), so LLM
//     flow/model/tool spans export with zero Genkit-specific code — provided
//     Init runs before Genkit first creates a span.
//   - Errors: the sentry-go SDK. The OTel linking integration stamps each captured
//     error with the active span's trace/span id so errors and traces correlate.
//
// SentryDSN is the on/off switch. Empty ⇒ Init is a no-op returning a no-op
// shutdown: no SDK, no exporter, no global provider override, and the app behaves
// exactly as before (fail-open, mirroring the AnalyticsDSN pattern). A telemetry
// failure must never take down the API.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	sentry "github.com/getsentry/sentry-go"
	sentryotel "github.com/getsentry/sentry-go/otel"
	sentryotlp "github.com/getsentry/sentry-go/otel/otlp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// flushTimeout bounds how long shutdown waits for the sentry transport to drain
// its in-flight error events. Span flushing is bounded by the caller's context.
const flushTimeout = 2 * time.Second

// noopShutdown is returned whenever telemetry is disabled or fails to start, so
// callers can always defer the shutdown unconditionally.
func noopShutdown(context.Context) error { return nil }

// Init installs error monitoring + OpenTelemetry tracing when cfg.SentryDSN is
// set, and returns a shutdown func that flushes both the span exporter and the
// error transport. When the DSN is empty it is a no-op: it returns
// (noopShutdown, nil) and touches no global state.
//
// Call once, early in main — after logging.New (so failures are structured) and
// BEFORE anything that creates OTel spans (the DB pool, the gRPC stack, and
// especially Genkit, which binds to whatever global TracerProvider exists at
// first use). A non-nil error means telemetry could not start; the caller should
// log it and proceed with telemetry disabled (never fatal).
func Init(ctx context.Context, cfg *config.Config) (shutdown func(context.Context) error, err error) {
	if cfg == nil || cfg.SentryDSN == "" {
		return noopShutdown, nil
	}

	if err := sentry.Init(sentry.ClientOptions{
		Dsn:         cfg.SentryDSN,
		Environment: cfg.SentryEnvironment,
		Release:     cfg.SentryRelease,
		Debug:       cfg.SentryDebug,
		// Spans travel via the OTel pipeline + OTLP exporter below, not the
		// sentry-go native tracer, so leave the SDK's own tracing off to avoid
		// duplicate/competing transactions.
		EnableTracing: false,
		// D3: strip request bodies, cookies, query strings and auth headers from
		// error payloads before they leave the process.
		BeforeSend: scrubEvent,
		// Link captured errors/logs to the active OTel trace (trace/span id) so
		// an error opens onto its distributed trace in Sentry.
		Integrations: func(in []sentry.Integration) []sentry.Integration {
			return append(in, sentryotel.NewOtelIntegration())
		},
	}); err != nil {
		return noopShutdown, err
	}

	// Endpoint, URL path, headers and TLS mode are all derived from the DSN.
	exporter, err := sentryotlp.NewTraceExporter(ctx, cfg.SentryDSN)
	if err != nil {
		sentry.Flush(flushTimeout)
		return noopShutdown, err
	}

	// Resource identity attached to every span. resource.New only errors on
	// schema/detector conflicts, which plain attributes cannot trigger; tolerate
	// a partial resource rather than failing telemetry startup.
	res, rerr := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", cfg.OTelServiceName),
		attribute.String("service.version", cfg.SentryRelease),
		attribute.String("deployment.environment", cfg.SentryEnvironment),
	))
	if rerr != nil {
		slog.WarnContext(ctx, "telemetry: partial otel resource",
			logging.AttrComponent, "telemetry", logging.AttrError, rerr)
	}

	// Parent-based head sampling: a child span inherits its parent's decision, so
	// once an inbound HTTP span is sampled the whole downstream trace (Genkit, DB,
	// gRPC) is kept together; roots are sampled at the configured ratio. Wrapped
	// to drop parentless client spans (DB/gRPC calls made outside any request or
	// job trace) so background/boot query noise never reaches Sentry.
	sampler := parentlessClientDropSampler{
		base: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SentrySampleRate)),
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	// W3C tracecontext + baggage: continue an inbound trace and propagate it to
	// downstream services. (Sentry ingests W3C via OTLP; the browser SDK also
	// emits W3C traceparent alongside sentry-trace.)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Trace outbound HTTP that goes through the default transport — the Genkit
	// Anthropic plugin's model calls (it builds its client from
	// http.DefaultClient), plus Gemini/Zernio/Firecrawl/Resend — as client spans
	// that join the active trace and carry the traceparent header downstream.
	// Installed here so all telemetry wiring lives in one place; it composes
	// underneath the Anthropic tool-order/debug transport wraps installed later
	// (they chain onto whatever DefaultTransport is current). Only parented client
	// spans survive the sampler, so background HTTP never becomes standalone noise.
	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)

	slog.InfoContext(ctx, "telemetry enabled",
		logging.AttrComponent, "telemetry",
		"service", cfg.OTelServiceName,
		"environment", cfg.SentryEnvironment,
		"sample_rate", cfg.SentrySampleRate)

	return func(ctx context.Context) error {
		// Flush spans first (bounded by ctx), then drain the error transport.
		err := tp.Shutdown(ctx)
		sentry.Flush(flushTimeout)
		return err
	}, nil
}

// CaptureError reports err to Sentry, linked to the active OTel trace carried by
// ctx (via the OTel integration's context resolver, which reads the span from
// the event hint's context). Extra attributes are attached to the event.
//
// Safe to call unconditionally: when telemetry is disabled the current hub has
// no client and this is a no-op. Use it on paths that have no HTTP middleware to
// report for them — Genkit rebuild failures, background jobs, boot.
func CaptureError(ctx context.Context, err error, attrs ...attribute.KeyValue) {
	if err == nil {
		return
	}
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		hub = sentry.CurrentHub()
	}
	client, scope := hub.Client(), hub.Scope()
	if client == nil {
		return // telemetry disabled
	}
	// Clone so per-error attributes never leak onto the shared scope.
	s := scope.Clone()
	if len(attrs) > 0 {
		c := make(sentry.Context, len(attrs))
		for _, a := range attrs {
			c[string(a.Key)] = a.Value.AsInterface()
		}
		s.SetContext("telemetry", c)
	}
	// Pass ctx on the hint so the OTel integration can resolve trace/span id.
	client.CaptureException(err, &sentry.EventHint{OriginalException: err, Context: ctx}, s)
}

// scrubEvent (D3) removes request bodies, cookies, query strings and sensitive
// headers from an error event before it is sent. Structured metadata (tags,
// extras, the URL path, and the linked trace ids) is kept.
func scrubEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event.Request != nil {
		event.Request.Cookies = ""
		event.Request.QueryString = ""
		event.Request.Data = ""
		for k := range event.Request.Headers {
			switch k {
			case "Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "Sentry-Trace", "Baggage":
				delete(event.Request.Headers, k)
			}
		}
	}
	return event
}
