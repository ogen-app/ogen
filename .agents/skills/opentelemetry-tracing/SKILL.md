---
name: opentelemetry-tracing
description: Wire, debug, or extend the OpenTelemetry tracing + Sentry error-monitoring stack in the Ogen API (and understand how the UI must propagate into it), distilled from CON-302/303/304. Covers the single wiring point (telemetry.Init), Sentry-via-OTLP export, the parentless-client-drop sampler, the sentry-trace↔W3C propagator, and the two traps that make spans silently vanish: Fiber's c.Context() vs c.UserContext() span-parenting split (use reqCtx/detachedContext) and the browser's sentry-trace-not-traceparent header. Use when the user says traces are missing DB/gRPC/HTTP spans, a trace shows "ui → api but nothing below", spans aren't nested/parented, sentry-trace / traceparent / baggage propagation is broken, they're adding OTel instrumentation to a new handler/job/service/client, tuning sampling, or debugging why Sentry shows disconnected traces.
tools: Read, Edit, Write, Glob, Grep, Bash
---

# OpenTelemetry Tracing + Sentry Skill

How Ogen does distributed tracing and error monitoring, distilled from **CON-302**
(umbrella), **CON-303** (API) and **CON-304** (UI). Everything is **vendor-neutral
OpenTelemetry** in the code; exactly one package decides where the signals go.

**Mental model of one trace:**

```
[browser pageload/nav]  (@sentry/react, sends `sentry-trace` + `baggage`)
        │  http.client GET /api/...
        ▼
[otelfiber server span]  (continues the browser trace via sentryTracePropagator)
        ├─ bun query span         (bunotel, parented via reqCtx(c))
        ├─ otelgrpc client span    → [pdf/video/image/audio/documents service]
        ├─ otelhttp client span    (Gemini / Zernio / Firecrawl / Resend)
        └─ Genkit flow/model/tool spans (reuse the global TracerProvider)

[River job]  → its OWN root span "job <kind>" (no inbound request to continue)
        └─ bun / gRPC / HTTP child spans
```

Two independent traps break this and produce the classic symptoms; both have bitten
us and both are covered below:

1. **`c.Context()` vs `c.UserContext()`** — the server span is only on
   `UserContext`; passing `c.Context()` to the DB/gRPC layer orphans every child
   span and the sampler drops it → *"I see `api` but nothing under it."*
2. **`sentry-trace` ≠ W3C `traceparent`** — the browser SDK and cross-origin
   `tracePropagationTargets` → *"ui and api are two separate traces."*

## Canonical references to diff against

- **Wiring (the one place):** `src/kernel/telemetry/telemetry.go` — `Init()`, exporter,
  sampler install, `otel.SetTextMapPropagator(Propagator())`, `otelhttp` default transport.
- **Propagator:** `src/kernel/telemetry/propagator.go` — `Propagator()` composite +
  `sentryTracePropagator`.
- **Sampler:** `src/kernel/telemetry/sampler.go` — `parentlessClientDropSampler`.
- **Error capture + scrub:** `src/kernel/telemetry/telemetry.go` (`CaptureError`, `scrubEvent`).
- **HTTP server chain:** `src/transport/server/observability.go` (`useObservability`,
  `injectTraceIDs`, `reportServerError`); registered in `src/transport/server/wiring.go`
  (`newFiberApp`, CORS `AllowHeaders`).
- **Request→data-layer context bridge:** `src/transport/handlers/tracing.go`
  (`reqCtx`, `detachedContext`, `graftRequestSpan`).
- **DB:** `src/infra/database/database.go:52`, `src/infra/database/analytics.go:65`
  (`bunotel.NewQueryHook`).
- **gRPC:** `src/transport/grpc/server/server.go:73` (`otelgrpc.NewServerHandler`),
  every `src/transport/grpc/client/*/client.go` (`otelgrpc.NewClientHandler`).
- **Jobs:** `src/jobs/tracing.go` (`Middleware`, root span per job); installed on the
  River client in `src/transport/server/server.go`.
- **Boot order:** `cmd/server/main.go` — `telemetry.Init` (~L55) runs **before**
  `server.New` (~L140).
- **Config:** `src/kernel/config/config.go` (`Sentry*`, `OTelServiceName`); `.env.example`.
- **Regression tests:** `src/transport/server/observability_test.go`,
  `src/transport/handlers/tracing_internal_test.go`,
  `src/kernel/telemetry/{propagator,sampler,telemetry}_test.go`.

---

## Architecture decisions (CON-302/303 — read first)

- **Unified in Sentry, fed by OTLP.** Traces go through the OTel SDK to a batch
  exporter built by `sentryotlp.NewTraceExporter(ctx, cfg.SentryDSN)` (endpoint + auth
  derived from the DSN). Errors go through the sentry-go SDK. `sentry.Init` sets
  `EnableTracing: false` — spans travel the OTel pipeline, NOT sentry-go's native
  tracer, so there are no duplicate/competing transactions.
- **`SENTRY_DSN` is the single on/off switch.** Empty ⇒ `Init` is a **no-op** that
  returns a no-op shutdown and touches no global state (no exporter, no global
  `TracerProvider`, no propagator). Fail-open: a telemetry failure must never take
  down the API. **This is the #1 thing to check when there are no backend spans.**
- **The global `TracerProvider` is a concrete `*sdktrace.TracerProvider`.** Genkit
  binds to whatever global provider exists at first span, so LLM flow/model/tool
  spans export with **zero Genkit-specific code** — *provided `Init` runs first*.
- **Conservative capture.** `scrubEvent` (D3) strips request bodies, cookies, query
  strings and auth headers from error payloads before they leave the process.
- **Errors link to traces.** `sentryotel.NewOtelIntegration()` stamps each captured
  error with the active span's trace/span id so an error opens onto its trace.

---

## Boot order (non-negotiable)

`telemetry.Init(ctx, cfg)` must be called **early in `main`** — after `logging.New`,
and **before anything that creates a span or captures the global propagator**:

- before the DB pool, the gRPC stack, and **Genkit** (binds the global provider once);
- before `server.New` builds the Fiber app, because **`otelfiber.Middleware` captures
  `otel.GetTextMapPropagator()` at construction time**. Build the app before `Init`
  and otelfiber freezes the default no-op propagator → inbound `sentry-trace` is never
  parsed → every request starts a fresh root trace.

`main.go` already orders this correctly (`Init` ~L55, `server.New` ~L140). Don't
reorder them.

---

## The propagation story (`sentry-trace` ↔ W3C)

`Propagator()` (`propagator.go`) is a **composite**, and on Extract the composite runs
each member in order where **the last valid extractor wins**:

```go
propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},   // W3C traceparent  (server↔server, downstream inject)
    sentryTracePropagator{},      // reads the browser's `sentry-trace`  (LAST → wins)
    propagation.Baggage{},
)
```

- **The browser SDK (`@sentry/react`) sends `sentry-trace` + `baggage`, NOT W3C
  `traceparent`.** And **sentry-go v0.49 removed its built-in OTel propagator**, so
  without `sentryTracePropagator` nothing on the API understands the browser header and
  the UI→API trace is severed.
- `sentryTracePropagator.Extract` parses the header with
  `sentry.ParseTraceParentContext(...)` and returns a **remote** span context that
  otelfiber adopts as the server span's parent. `Inject` is a **no-op** — downstream
  (gRPC microservices, LLM HTTP) read W3C `traceparent`, emitted by
  `propagation.TraceContext` in the same composite.
- **Sampled-flag subtlety:** the remote parent is marked sampled **only** when the
  header's trailing flag is `SampledTrue` (`...-1`). `SampledFalse`/undefined stays
  unsampled and the parent-based sampler **drops the whole backend trace** (matching
  W3C behaviour). A trace that shows in Sentry was sampled at the browser, so its
  header ends in `-1`; if you ever see a `-0`, that's why nothing downstream appears.
- **CORS already allows it:** `wiring.go` sets
  `AllowHeaders: "Content-Type,sentry-trace,baggage,traceparent,tracestate"`. If you
  fork the CORS config, keep these or cross-origin requests can't send the header.

### UI side (CON-304) — the half that lives in the `ui` repo

`@sentry/react` only attaches `sentry-trace`/`baggage` to requests whose URL matches
**`tracePropagationTargets`** (default: same-origin + `localhost`). If the API is a
**different origin** than the app (typical on Railway: `app.…` vs `api.…`), the header
is **never sent** and the API starts its own root trace no matter how correct the
propagator is. Fix in the UI's Sentry init by adding the API origin to
`tracePropagationTargets`. Verify in DevTools → Network → the `/api/...` request →
Request Headers → `sentry-trace` present, value ends in `-1`.

---

## The context-threading trap (Fiber `c.Context()` vs `c.UserContext()`)

**This is the subtle one — it makes DB/gRPC spans vanish while the server span shows.**

A Fiber request carries two different contexts:

| accessor | is it | carries the OTel span? | carries Locals + cancellation? |
|---|---|---|---|
| `c.UserContext()` | a Go `context.Context` | **yes** — otelfiber does `c.SetUserContext(ctx)` | no |
| `c.Context()` | the `*fasthttp.RequestCtx` | **no, and it CAN'T** — its `Value()` only reads Locals; OTel's span key is unexported | yes (log-correlation ids via Locals; client-disconnect cancel) |

The codebase passes `c.Context()` into the service/repo layer **everywhere** (it's how
log correlation works: `injectTraceIDs` puts trace/span ids in Locals, read back via
`c.Context().Value`). But a `bunotel` / `otelgrpc` / `otelhttp` call made with a bare
`c.Context()` starts a span with **no parent** → a **parentless client span** →
`parentlessClientDropSampler` drops it. Net effect: DB and gRPC work never appears
under the request.

**The fix (already in `handlers/tracing.go`): graft the span onto the request context.**

```go
// SYNCHRONOUS request path — hand THIS to repos / gRPC / HTTP clients, not c.Context().
func reqCtx(c *fiber.Ctx) context.Context { return graftRequestSpan(c.Context(), c) }

// DETACHED path — SSE StreamWriters run after the handler returns and Fiber recycles
// c, so start from context.Background() and copy tenant/request-id + the span.
func detachedContext(c *fiber.Ctx, tenantID string) context.Context { /* … */ }

// The shared move: copy the server span (from UserContext) onto ctx. The span
// context is an immutable value (trace/span id, sampled flag), so copying it is safe;
// children only need the parent's ids, not a live parent span.
func graftRequestSpan(ctx context.Context, c *fiber.Ctx) context.Context {
	if sc := trace.SpanContextFromContext(c.UserContext()); sc.IsValid() {
		return trace.ContextWithSpanContext(ctx, sc)
	}
	return ctx
}
```

`reqCtx(c)` is a **strict superset** of `c.Context()` — it still delegates
`Value`/`Deadline`/`Done` to the fasthttp ctx, so logging and cancellation are
unchanged; it only *adds* the span. **Rule of thumb:** in a handler, pass `reqCtx(c)`
to anything that reaches the DB, a gRPC client, or an outbound HTTP client; keep
`c.Context()` only for reading Locals or calling concrete `*fasthttp.RequestCtx`
methods (`c.Context().SetBodyStreamWriter(...)`).

---

## The sampler (why "harmless noise suppression" hides bugs)

`parentlessClientDropSampler` (`sampler.go`) wraps
`ParentBased(TraceIDRatioBased(SentrySampleRate))` and:

- **drops** a span that is **client-kind AND has no valid parent** — kills boot
  migrations / background analytics writes / a query in a job before it opened its span
  from becoming noisy one-span "traces";
- **keeps** parentless **server** spans (otelfiber's inbound span) and **internal**
  spans (Genkit/job roots);
- defers everything else to `ParentBased` — so a child of a **sampled** parent is
  always kept (the whole downstream trace rides the inbound decision).

**Consequence to remember:** this sampler is exactly what turns the `c.Context()` bug
into *silence* rather than *orphan traces*. Loosening the sampler is **not** the fix —
it would just surface each DB query as its own one-span root trace. The fix is always
**parenting** (`reqCtx`).

---

## Adding instrumentation to a new component

- **New handler / endpoint** → nothing to wire; it already sits under otelfiber. Just
  pass **`reqCtx(c)`** (not `c.Context()`) into repos/clients. If it's SSE or spawns
  work that outlives the request, use **`detachedContext(c, tenantID)`**.
- **New DB pool** → `db.AddQueryHook(bunotel.NewQueryHook(bunotel.WithDBName("<name>")))`.
- **New gRPC client** → `grpc.WithStatsHandler(otelgrpc.NewClientHandler())` on the dial
  (mirror `src/transport/grpc/client/*/client.go`). Server side already has
  `otelgrpc.NewServerHandler()`; the W3C `traceparent` is injected for you.
- **New outbound HTTP** → if it uses `http.DefaultTransport` it's already traced
  (`telemetry.go` wraps it with `otelhttp.NewTransport`). A custom `http.Client` needs
  its own `Transport: otelhttp.NewTransport(base)`.
- **New River job / queue** → nothing; `jobs.Middleware()` gives every job a root span
  `"job <kind>"`. Just thread the `ctx` you're handed into the data layer.
- **Manual span** → `otel.Tracer("ogen/<area>").Start(ctx, "<name>", …)`; always start
  from a ctx that already has a parent (`reqCtx(c)`, the job ctx, or a detached ctx).
- **Report a non-panic error to Sentry** → `telemetry.CaptureError(ctx, err, attrs…)`;
  it's a no-op when disabled and links to the active trace. Panics are already captured
  by `sentryfiber` (request) / exhausted-retry logic (jobs).

---

## Config / env

| env | field | default | notes |
|---|---|---|---|
| `SENTRY_DSN` | `SentryDSN` | `""` | **on/off switch**; empty ⇒ all telemetry off (no-op) |
| `SENTRY_ENVIRONMENT` | `SentryEnvironment` | `development` | shown as the env tag in Sentry |
| `SENTRY_RELEASE` | `SentryRelease` | `""` | release for source-map/version grouping |
| `SENTRY_TRACES_SAMPLE_RATE` | `SentrySampleRate` | `0.1` | **root** sample ratio; children of a sampled parent are always kept |
| `SENTRY_DEBUG` | `SentryDebug` | `false` | sentry-go SDK debug logging |
| `OTEL_SERVICE_NAME` | `OTelServiceName` | `ogen-api` | resource `service.name` |

The backend logs `telemetry enabled` (once, at boot) only when the DSN is set; the
disabled path logs nothing. **Absence of that line = telemetry is off.**

---

## Debugging playbook: "I don't see spans"

Work top-down; each answer tells you which layer is broken.

1. **No backend spans at all?** Grep the API boot logs for `telemetry enabled`. Missing
   ⇒ `SENTRY_DSN` is empty on *that* environment (Railway vars, not the local `.env`).
   Set it and restart.
2. **UI and API are two separate traces?** The `sentry-trace` header isn't making it
   from browser to the otelfiber `Extract`:
   - DevTools → Network → `/api/...` → Request Headers → is `sentry-trace` present?
     **Absent** ⇒ UI `tracePropagationTargets` doesn't include the API origin
     (cross-origin), or a proxy strips it. Fix in the `ui` repo.
   - Present but a fresh root still starts ⇒ backend can't parse it: confirm
     `Propagator()` is installed (`telemetry.go`) and `Init` ran before the app was
     built. Header ends in `-0`? Trace was unsampled at the browser → parent-based drop.
3. **Server span shows but nothing under it (no DB/gRPC)?** The classic `c.Context()`
   trap — the handler passed `c.Context()` into the data layer, so the child was a
   parentless client span the sampler dropped. Switch that call to **`reqCtx(c)`** (or
   `detachedContext` for SSE). This is `handlers/tracing_internal_test.go`'s regression.
4. **A background job's work is missing?** It should have a root span `"job <kind>"`;
   if the job's DB call is missing, it ran with a ctx that lost the job span — thread
   the handed-in `ctx` all the way down.

---

## Gotchas (CON-303/304 lessons)

1. **`SENTRY_DSN` empty = totally silent.** No error, no log, no spans. Always confirm
   it's set on the environment that served the trace before debugging anything deeper.
2. **`c.Context()` cannot carry the OTel span, ever.** It's a `*fasthttp.RequestCtx`;
   its `Value()` only reads Locals. Use `reqCtx(c)` for the data layer. This is *the*
   reason DB spans go missing while the server span shows.
3. **The browser speaks `sentry-trace`, not `traceparent`.** Needs `sentryTracePropagator`
   on the API and the API origin in the UI's `tracePropagationTargets`. Both, or the
   trace stays split.
4. **`Init` must precede the Fiber app build.** otelfiber snapshots the global
   propagator at construction; build first and it freezes the no-op.
5. **The sampler makes orphans *silent*.** Missing spans usually mean "parentless +
   dropped," not "not created." Fix parenting, don't disable the sampler.
6. **Sampled bit propagates the decision.** A child of a sampled parent is always kept
   regardless of `SENTRY_TRACES_SAMPLE_RATE`; a `-0` `sentry-trace` header drops the
   whole downstream trace by design.
7. **Don't turn on sentry-go native tracing.** `EnableTracing` stays `false`; spans
   come from the OTel pipeline. Turning it on double-counts transactions.
8. **Custom `http.Client`s aren't traced automatically.** Only `http.DefaultTransport`
   is wrapped. Wrap your own transport with `otelhttp.NewTransport`.
9. **Genkit needs `Init` first.** It binds to the global provider at first span; if
   Genkit traces before `Init`, its spans export nowhere.

---

## Checklist

- `telemetry.Init` runs before DB/gRPC/Genkit **and** before `server.New`.
- `SENTRY_DSN` set on the target environment (else expect zero backend spans).
- Handlers pass **`reqCtx(c)`** into repos/gRPC/HTTP; `detachedContext` for SSE/detached
  work; bare `c.Context()` only for Locals or concrete fasthttp calls.
- New DB pool has `bunotel.NewQueryHook`; new gRPC client has `otelgrpc.NewClientHandler`;
  custom HTTP client wraps `otelhttp.NewTransport`.
- Propagator composite includes `sentryTracePropagator` (browser) + `TraceContext`
  (downstream) + `Baggage`; CORS `AllowHeaders` keeps `sentry-trace,baggage`.
- UI `tracePropagationTargets` includes the API origin (cross-origin deploys).
- Sampling change? Confirm you're not turning parentless drops into orphan traces —
  fix parenting instead.
- Regression coverage: a child span from `reqCtx(c)` parents under the otelfiber server
  span (`tracing_internal_test.go`); server span continues an inbound `sentry-trace`
  (`observability_test.go`).
- `make test` green; a real request in Sentry shows `server → bun/gRPC/HTTP` nested.
