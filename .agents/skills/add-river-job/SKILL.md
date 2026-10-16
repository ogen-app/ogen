---
name: add-river-job
description: Add a background job to Ogen now that jobs run in river-service — the proto args message, the ogen side (enqueue through the outbox, the job's steps or its maintenance pass), the river-service worker, periodic scheduling, and tests. Use when the user asks to add an async/background task, a queue worker, a recurring sweep, polling, or to offload work out of a request handler.
tools: Read, Edit, Write, Glob, Grep, Bash
---

# Add River Job Skill

Background jobs run in **river-service** (a separate repo and Railway service,
CON-332), on open-source River. ogen keeps every decision and every write
next to its data; river-service owns the queue, the schedule and the outside
calls. A new job touches three repos:

| Where | What |
|---|---|
| `ogen-app/proto` | the args message in `ogen.jobs.v1` (one per kind, field for field with the Go task) |
| `ogen` (this repo) | the Go task, its enqueue method, and either its **steps** (`JobStepService`) or its **maintenance pass** (`MaintenanceService`) |
| `ogen-app/river-service` | the args type, its registration and schedule — see that repo's `add-step-job` skill |

ogen has no job queue of its own: every enqueue is a `job_outbox` row the
relay delivers to river-service. Tests run a job's steps inline (`runLocal`,
behind each processor's `Process`), the same steps river-service drives.

Canonical references in `src/jobs/queues/`:

- `job_steps.go` — the step protocol: `StepInput`, `StepOutput`, actions,
  `done`/`retry`/`snooze`, `JobSteps` registration.
- `inline_steps.go` — `runLocal`, the inline step runner behind `Process`,
  which the tests use.
- `withdraw_zernio_post.go`, `post_first_comment.go` — one Zernio call.
- `poll_zernio_status.go` — a call, then snooze until terminal.
- `submit_post_to_zernio.go` — a multi-phase machine (retry, presign +
  transfer per attachment, submit, dedupe paging, withdraw).
- `refresh_zernio_analytics.go`, `sweep_zernio_orphans.go` — a periodic sweep
  that fans out into one child job per tenant.
- `process_pdf.go`, `process_audio.go` — sidecar calls and the staged embed
  loop (`ingest_steps.go`).
- `analyze_asset.go` — work run **detached** in ogen with a `Wait` step.
- `maintenance.go` — sweeps that only touch ogen's tables.
- `jobs.go` — `Deps`, `JobArgs`, `EnqueueOpts` and the outbox `Enqueuer`.

## Step 0: Clarify scope

1. **Kind** — stable snake_case (`submit_post_to_zernio`). It is the River
   kind in both services and the proto oneof field.
2. **One-shot or periodic?** Periodic sweeps are River `PeriodicJob`s
   scheduled by river-service's leader, never self-rescheduling copies.
3. **Does it call anything outside ogen** (Zernio, Gemini, Firecrawl, a
   sidecar, an outside URL, a media upload)?
   - **No** → a *maintenance pass*: river-service schedules it, ogen runs it
     whole through `MaintenanceService.Run` (Step 3b).
   - **Yes** → a *step-run job*: ogen plans each outside call and decodes its
     result; river-service makes it (Step 3a).
4. **Payload** — ids only; every step re-loads the rows.
5. **Enqueue site** — transactional with a DB write? (Almost always yes.)
6. **Long work?** Every step must finish well inside the drain window (about
   4 minutes); long work is split into steps with checkpoints stored in ogen.

---

## Step 1: The args message (`ogen-app/proto`)

Add `message <Kind>Args` to `proto/jobs/v1/jobs.proto` and a field to
`EnqueueRequest.args`. Additive only: never renumber or remove a field (the
deploy order below relies on old readers ignoring new fields). Tag a release
(`vX.Y.0`) and bump `PROTO_VERSION` in both Makefiles; I can't push tags, so
ask the user.

In ogen, map the Go task to the message in `outbox_args.go` (`setArgs`).
Add the task to the table in `outbox_args_test.go`, which checks every kind
round-trips to its message and is relayable.

---

## Step 2: The Go task and its enqueue (`src/jobs/queues/<kind>.go`)

```go
const <Kind>Queue = "<kind>"

// <Kind>Task carries ids; the steps re-load the rows.
type <Kind>Task struct {
	PostID string `json:"post_id"`
}

// Kind implements JobArgs. Queue, attempts and uniqueness are set on the
// river-service args type.
func (<Kind>Task) Kind() string { return <Kind>Queue }
```

Enqueue through the `Enqueuer`, inside the caller's transaction:

```go
func (e *Enqueuer) Enqueue<Kind>Tx(ctx context.Context, tx *sql.Tx, postID string) error {
	return e.insertTx(ctx, tx, <Kind>Task{PostID: postID}, nil)
}
```

`insertTx` writes a `job_outbox` row in `tx`; the trace carrier and request
id ride along automatically. Schedule with `&EnqueueOpts{ScheduledAt: …}`. No
transaction: `e.insert`. From inside a job, where losing the follow-up
matters: `e.enqueue` (fails when nothing is wired). Consumers depend on a
narrow interface in **their** package, never on the outbox.

---

## Step 3a: A step-run job

A job is a function `step(ctx, StepInput) (StepOutput, error)`. river-service
calls it, does what it returns, and calls it again with the raw result:

| `StepOutput` | river-service does |
|---|---|
| `HTTP` (`Upstream` `zernio`/`gemini`/`firecrawl` + path, or an absolute `URL`) | the call with that upstream's leased key; absolute URLs go through its SSRF guard |
| `Sidecar` (unary, or `Stream` from a stored file) | the gRPC call to pdf/documents/image/audio-service |
| `Transfer` | streams a stored file to an upload URL |
| `Wait` | sleeps, then steps again with the same state |
| `Done` (+ `Enqueue` child jobs, `Cancel` job matches) | ends the job |
| `Retry` (`After` > 0 = snooze) | retries (costs an attempt) or snoozes (doesn't) |

Plan and decode with the ordinary clients, so request building and error
mapping keep one implementation:

```go
cancel := func(c *zernio.Client) error { return c.Cancel(ctx, id) }
if in.Result == nil {
	call, err := zernio.Plan(cancel)   // capture, don't send
	if err != nil { return StepOutput{}, err }
	return nextZernio(nil, call)
}
if zernioDisabled(in.Result) { return done("skipped_zernio_disabled") }
cancelErr := zernio.Decode(zernioResult(in.Result), cancel) // the error a direct call would return
```

- Paging reads: `zernio.Replay(prior, op)` returns the next page's request.
- Sidecars: `sidecarStep(state, service, timeout, stream, op, local)` with the
  typed client's `Over(cc)` constructor; decode with `sidecarResult` inside
  `localOr`.
- Firecrawl: `firecrawl.Plan` / `firecrawl.Decode`. Gemini embeds: the
  staged `embedLoop` (`ingest_steps.go`).
- Give an action whose client is ogen's own (a sidecar, the embedder) a
  `local` closure, and read the outcome with `localOr`: a run inside ogen
  then calls that client directly, and the tests' fakes keep working.

**State.** `StepOutput.State` comes back in the next `StepInput`. Keep it small
(ids, cursors, a few short strings); it is lost on a snooze or retry, which
restart the job from its first step. Large intermediate data goes into
ogen tables keyed by a run id (`asset_chunk_stages`, extraction rows). Every
flow must converge when it runs again from the start.

**Attempts.** `in.lastAttempt()` tells the last attempt, when a transient
failure must be settled instead of retried.

**Long calls ogen makes itself** (a model call): run them detached and poll
with `Wait`, tracking them on a row ogen owns (`assetanalysis.StartDetached`
and the lens run slot). A restarted job adopts the run in flight; a stale
heartbeat retries.

Register the step in `NewJobSteps` (`job_steps.go`), and give the processor a
`Process` that runs it inline for the tests:

```go
func (p *<Kind>Processor) Process(ctx context.Context, task <Kind>Task) error {
	return runLocal(ctx, p.step, <Kind>Queue, task, 0, 0, localActions{zernio: p.Deps.Client})
}
```

**Fan-out sweeps** return `done("fanned_out", children...)` with one
`ChildJob` per tenant; the child kind exists only between the two services
(river-service registers it as internal). Inline runs work them through
`localActions.children`.

---

## Step 3b: A maintenance pass

A sweep that touches only ogen's tables (cleanups, reconcile,
manual-publish-due, the analysis backfill) stays an ordinary processor and is
listed in `MaintenanceTasks` (`maintenance.go`) under its kind and timeout;
the pass runs under a system tenant scope. Give it a `new<Kind>(d Deps)`
constructor.
Update `TestMaintenanceTasksMatchTheContract` and the task list in
`jobrpc.proto`'s `RunRequest` comment.

---

## Step 4: Periodic scheduling

river-service schedules every sweep (`queue.RegisterPeriodic` with a cadence
env var, `sweepOpts()` for active-state uniqueness); nothing in ogen changes.

---

## Step 5: Tests

- **Inline run:** the processor's `Process`/`process` with fakes (it runs
  the steps through `runLocal`; a snooze comes back as `*SnoozeError`).
- **river-service mode:** drive `p.step` with raw results, no local closures
  (`driveSteps` in `ingest_steps_test.go`, `runSteps` + `scriptedZernio` in
  `zernio_lifecycle_steps_test.go`).
- **Mapping:** `src/transport/grpc/jobrpc/steps_test.go` for new action
  fields.
- DB tests (`pgtest`) for new repository methods.

Run `go test ./src/jobs/... ./src/transport/grpc/jobrpc/...` and `make lint`.

---

## Deploy order (every new kind)

1. Tag the proto release.
2. ogen with the task, its steps or maintenance pass. Until river-service
   knows the kind it refuses it at `Enqueue`, so the rows wait in the
   outbox: ship river-service right after, or enqueue nothing yet.
3. river-service with the args type and registration.

A step river-service asks for before ogen has it comes back `UNIMPLEMENTED`,
and the job snoozes until ogen catches up.

## Gotchas

1. **Never read River's tables from ogen** (`river_job`): they live in
   river-service's schema. Track job progress on rows ogen owns.
2. **A retry or snooze restarts the job** at its first step with empty state.
3. **`Retry{After: 0}` costs an attempt**; use `snooze()` to wait without one.
4. **`omitzero`** for numeric/bool state fields, `omitempty` for strings,
   slices and maps.
5. **No storage credentials in river-service:** hand it presigned links
   (`sourceURL`, `PresignedPutURL`).
6. **Attempt timeouts stay below river-service's `RESCUE_STUCK_JOBS_AFTER`
   (30 min).** Longer work checkpoints and resumes across attempts.
