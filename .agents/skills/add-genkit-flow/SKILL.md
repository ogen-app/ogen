---
name: add-genkit-flow
description: Add a new Genkit-powered AI flow (Anthropic-backed) to the app — types, prompt template, run function, tools, context cache, HTTP handler, tests, and optional SSE streaming with per-field delta events. Use when the user asks to build an AI assistant, generator, classifier, or any other LLM-driven feature that needs prompt templates, structured output, tool use, or progress streaming to the UI.
tools: Read, Edit, Write, Glob, Grep, Bash
---

# Add Genkit Flow Skill

Scaffold a Genkit flow package with the canonical layering used by `post_assistant` and `content_plan`. Follow this exactly — the project already has two flows to diff against; deviating costs more than it saves. Canonical references:

- `src/genkit/flows/post_assistant/` — conversational, tools, SSE streaming with per-field deltas
- `src/genkit/flows/content_plan/` — multi-step pipeline, SSE streaming of typed events (step / post)

## Step 0: Clarify scope

Before writing any code, confirm with the user:

1. **Flow name** in camelCase (e.g. `postAssistant`, `generateContentPlan`). This becomes the Genkit flow name and the package name (snake_case: `post_assistant`).
2. **Structured output shape** — the fields the model must produce. Which are strings, which are enums, which are arrays?
3. **Streaming?** — does the client need live progress?
   - If yes: what granularity — **per-field text deltas** (like post_assistant) or **typed step/item events** (like content_plan)?
4. **Tools?** — what external data must the model fetch at its discretion (asset retrieval, search, etc.)? Tools are for model-driven lookups, not for mandatory context — put mandatory data in the prompt directly.
5. **Conversational?** — is there a message history per resource? If yes, where are messages persisted?
6. **HTTP route** — e.g. `POST /api/posts/:id/assistant`, `POST /api/campaigns/:id/generate-draft`.
7. **Error classes** — what counts as a 400 (bad input / not found) vs 502 (model failure) vs 500 (internal)?

Do not proceed until 1–4 and 6 are clear. Streaming granularity (3) drives half the code.

---

## Step 1: Package layout 

```
src/genkit/flows/<flow_name>/
├── types.go                    # request/response, SSE events, errors, repos bundle, flow config
├── flow.go                     # InitX, NewXCallback, emit helper
├── run.go                      # runX — the actual flow logic
├── context.go                  # prompt context assembly + fingerprint cache (if static context)
├── tools.go                    # ai.ToolRef bundle + implementations (if tools)
├── scanner.go                  # incremental JSON string-value scanner (if per-field streaming)
├── scanner_test.go             # scanner unit tests (pure Go, no Ginkgo)
└── prompts/
    └── <flow_name>.tmpl        # two {{define "system"}} + {{define "context"}} blocks
```

Server wiring lives in a thin file `src/transport/server/<flow_name>.go` (mirrors `src/transport/server/content_plan.go` and `src/transport/server/post_assistant.go`). HTTP handler lives in `src/transport/handlers/<resource>.go` — usually co-located with the parent resource (posts → posts.go, campaigns → campaigns.go).

---

## Step 2: Types (`types.go`)

Every flow needs these. Copy the template, then trim what you don't use.

```go
package <flow_name>

import (
    "github.com/firebase/genkit/go/ai"

    "github.com/ogen-app/ogen/src/infra/repository"
)

// <FlowName>Request is the input to the flow.
type <FlowName>Request struct {
    // e.g. PostID string `json:"postId"`
    // e.g. Instruction string `json:"instruction"`
}

// <FlowName>Response is the structured output from the flow.
//
// Field order matters: Anthropic's structured-output decoding emits keys in
// the order declared by the jsonschema tags, which is the struct declaration
// order. Put user-visible streaming fields (explanation, content) first so
// they start arriving before metadata fields.
type <FlowName>Response struct {
    // e.g. Explanation    string `json:"explanation"    jsonschema:"description=..."`
    // e.g. UpdatedContent string `json:"updatedContent" jsonschema:"description=..."`
    // e.g. Action         string `json:"action"         jsonschema:"description=...,enum=edited,enum=declined"`
}

// <FlowName>Repos bundles all repository dependencies. Always bundle via a
// single struct — the flow takes ONE repos arg, not N.
type <FlowName>Repos struct {
    // e.g. Posts    repository.PostRepository
    // e.g. Assets   repository.AssetRepository
}

// <FlowName>FlowConfig holds static settings (token caps, optional embedder).
// The model is not here: it is resolved per call from the flow's slot.
// Not per-request state — that goes in context.WithValue.
type <FlowName>FlowConfig struct {
    // MaxOutputTokens caps the model's output for one call. 0 falls back
    // to 64000; CallConfig clamps it to the resolved model's max output
    // (64K on Claude 4.x, 128K on 5.x). Anthropic charges
    // only for tokens actually emitted, so a generous cap costs nothing
    // on short responses but prevents truncation when explanation + full
    // content + tool inputs combined exceed a smaller cap. Detect
    // truncation deterministically via resp.FinishReason ==
    // ai.FinishReasonLength (see run.go template).
    MaxOutputTokens int64
    // MaxTurns caps tool-use round-trips. With tools, the model needs
    // N+1 turns to make N tool calls plus 1 final answer. 0 = sensible
    // default (8). Genkit's library-wide default is 5; we recommend 8 so
    // realistic asset-incorporation flows don't blow the cap.
    MaxTurns int
    Embedder ai.Embedder // nil = semantic search unavailable
}

// ── Errors (mapped to HTTP codes by the handler) ────────────────────────────

// ValidationError → HTTP 400. Use for bad input, missing records, permission.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError → HTTP 502. Use only for model-call failures.
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE event types (omit this block if the flow is not streaming) ──────────

type SSEEventKind string

const (
    // Pick the shape that fits the flow. Two common patterns:
    //   per-field-delta flow:   explanation_delta, content_delta, tool_call, tool_result
    //   multi-step flow:        step, post (or item)
    // Every streaming flow also emits: complete, error.
    SSEEvent<Kind>   SSEEventKind = "<kind>"
    SSEEventComplete SSEEventKind = "complete"
    SSEEventError    SSEEventKind = "error"
)

// DeltaEventPayload — used by per-field-delta flows.
type DeltaEventPayload struct {
    Delta string `json:"delta"`
}

// StepEventPayload — used by multi-step flows (one per named step).
type StepEventPayload struct {
    Step   string `json:"step"`
    Status string `json:"status"` // always "done" — start/fail states aren't useful
}

// ToolCallEventPayload / ToolResultEventPayload — used when the flow exposes
// model tool calls to the UI. Omit the tool's output bytes in the result
// event; the UI only needs a "done" signal to resolve the chip.
type ToolCallEventPayload struct {
    Name  string `json:"name"`
    Input any    `json:"input,omitempty"`
    Ref   string `json:"ref,omitempty"`
}

type ToolResultEventPayload struct {
    Name string `json:"name"`
    Ref  string `json:"ref,omitempty"`
    OK   bool   `json:"ok"`
}

type ErrorEventPayload struct {
    Message string `json:"message"`
    Code    int    `json:"code"` // HTTP semantic: 400, 502, 500
}

// OnEventFunc is an optional callback invoked as SSE events are produced.
// A nil OnEventFunc is valid — the flow runs silently (Genkit Dev UI path).
type OnEventFunc func(name SSEEventKind, data any)
```

**Rules:**
- `jsonschema:"description=..."` is mandatory on every response field — Anthropic uses these to constrain output.
- Keep `Embedder` even if unused today; adding it later is trivial and tools generally need it.
- Never make errors untyped — the handler dispatches on `*ValidationError` / `*AIError` via `errors.As`.
- Enum fields: `jsonschema:"...,enum=v1,enum=v2"` — comma-separated `enum=` tags.

---

## Step 3: Prompt template (`prompts/<flow_name>.tmpl`)

Two blocks: `system` (stable cached prefix — role, rules, schema) and `context` (variable per-request data). Anthropic's server-side prompt cache uses exact prefix matching, so the system block should be identical across calls for the same flow type.

```go-template
{{define "system"}}You are the <role>.

## Your role
- <bullet list of what it does>

## Response format
You MUST respond with a single JSON object matching this exact schema.
Field order matters — emit keys in exactly the sequence shown:
{
  "explanation": "...",
  "updatedContent": "..."
}

Respond ONLY with the JSON object. No markdown fences, no extra text.{{end}}

{{define "context"}}## <Variable section title>

**Field**: {{.FieldName}}
{{- if .OptionalField}}
**Optional**: {{.OptionalField}}
{{- end}}
{{end}}
```

### Two response modes (when the flow edits a resource)

For flows that edit a resource (post, asset, campaign, etc.), users routinely send messages that are **questions** rather than edit requests — "what does the asset say about X?", "summarize the campaign", "how long is this post?". If the prompt only allows "edit" and "decline", the model frequently gives up on JSON entirely and writes prose. That triggers the pure-prose recovery path (see Step 6) but it's better to design for it explicitly.

Pattern: keep one JSON envelope and distinguish via `action`:

- `action: "edited"` — content changed, populate the long field.
- `action: "declined"` — no content change. Use this for **both** out-of-scope refusals AND informational answers; the `explanation` field carries the prose response in both cases.

Be explicit in the prompt about both modes, with examples. Reinforce that the model must **always** wrap output in the JSON envelope — even purely informational answers go inside `explanation`. See `src/genkit/flows/post_assistant/prompts/post_assistant.tmpl` for the canonical phrasing.

**Rules:**
- Embed via `//go:embed prompts/<flow_name>.tmpl` in `flow.go`.
- The template data struct (`contextTemplateData`) lives in `context.go`.
- Always instruct the model to respond with raw JSON, no fences. The parser in `run.go` tolerates fences defensively, but the prompt should ask for clean output.
- Never reference fields the Go struct doesn't have — they'll silently disappear on unmarshal.

---

## Step 4: Context assembly (`context.go`) — if the flow has static prompt context

Use the fingerprint cache pattern. A plain string-equality check on one field is a bug waiting to happen (see the bug fixed in commit history where asset-list changes weren't invalidating the cache).

```go
package <flow_name>

import (
    "bytes"
    "context"
    "fmt"
    "strings"
    "sync"
    "text/template"
    "time"

    "github.com/ogen-app/ogen/src/domain/models"
)

const contextCacheTTL = 5 * time.Minute

type assistantContext struct {
    SystemPrompt string // stable cached prefix
    ContextBlock string // variable per-request data
}

type contextCacheEntry struct {
    ctx         *assistantContext
    fingerprint string
    expiresAt   time.Time
}

var (
    contextCache   = map[string]*contextCacheEntry{}
    contextCacheMu sync.Mutex
)

// postFingerprint returns a stable key that changes whenever any
// prompt-affecting field of the resource changes. Add every field that
// feeds into the rendered context here — missing a field means stale
// prompts.
func <resource>Fingerprint(r *models.<Resource>) string {
    var b strings.Builder
    b.WriteString(r.Content)
    b.WriteByte('\x1f')
    b.WriteString(strings.Join(r.UsedAssetIDs, ","))
    b.WriteByte('\x1f')
    // ... more fields as needed
    return b.String()
}

func assembleContextCached(
    ctx context.Context,
    r *models.<Resource>,
    repos <FlowName>Repos,
    systemTmpl, contextTmpl *template.Template,
) (*assistantContext, error) {
    fp := <resource>Fingerprint(r)

    contextCacheMu.Lock()
    if entry, ok := contextCache[r.ID]; ok {
        if time.Now().Before(entry.expiresAt) && entry.fingerprint == fp {
            contextCacheMu.Unlock()
            return entry.ctx, nil
        }
        delete(contextCache, r.ID)
    }
    contextCacheMu.Unlock()

    actx, err := assembleContext(ctx, r, repos, systemTmpl, contextTmpl)
    if err != nil {
        return nil, err
    }

    contextCacheMu.Lock()
    contextCache[r.ID] = &contextCacheEntry{
        ctx:         actx,
        fingerprint: fp,
        expiresAt:   time.Now().Add(contextCacheTTL),
    }
    contextCacheMu.Unlock()

    return actx, nil
}
```

`assembleContext` renders the template from a `contextTemplateData` struct. Split independent I/O across goroutines with `sync.WaitGroup` when fetching multiple unrelated resources.

**Rules:**
- Unit-separator (`\x1f`) between fingerprint fields — no payload can smuggle itself into an adjacent slot.
- Every field read by the template MUST be in the fingerprint.
- TTL of 5 minutes aligns with Anthropic's server-side prompt cache TTL.
- Skip context.go entirely if the flow has no static prompt context (one-shot classifiers, for example).

---

## Step 5: Tools (`tools.go`) — if the model makes tool calls

Tools are model-driven lookups. Per-request state (IDs the model shouldn't have to guess) goes into `context.Context` via a package-private key; tools read it via `getRequestState`.

> ⚠️ **Tools have a large, hidden latency cost — read Gotcha #15 before shipping.** The plugin forces `strict: true` and randomizes tool order per request, which makes Anthropic recompile the tool-call grammar (~50s) on every call. The global tool-order stabilizer already fixes the chronic case, but a new tool-using flow must add a `PrewarmTools` flag + a background `prewarmToolCache` at init to also kill the cold first-request hit. Mirror `campaign_assistant`/`post_assistant`.

```go
package <flow_name>

import (
    "context"

    "github.com/firebase/genkit/go/ai"
    "github.com/firebase/genkit/go/genkit"
)

type ctxKey int

const requestStateKey ctxKey = iota

type requestState struct {
    // IDs the tool needs but shouldn't trust from the model
    postID   string
    assetIDs []string
    repos    <FlowName>Repos
    embedder ai.Embedder
}

func withRequestState(ctx context.Context, s *requestState) context.Context {
    return context.WithValue(ctx, requestStateKey, s)
}

func getRequestState(ctx context.Context) *requestState {
    return ctx.Value(requestStateKey).(*requestState)
}

type toolSet struct {
    listAssets        ai.ToolRef
    getAssetChunks    ai.ToolRef
    // ...
}

func defineTools(g *genkit.Genkit) *toolSet {
    list := genkit.DefineTool(g, "listAssets",
        "Returns the list of assets attached to the current resource.",
        func(ctx *ai.ToolContext, _ struct{}) ([]AssetInfo, error) {
            return toolListAssets(ctx)
        },
    )
    // ...
    return &toolSet{listAssets: list /* , ... */}
}
```

**Rules:**
- Tool names must match regex `^[a-zA-Z0-9_-]{1,64}$` (Anthropic constraint).
- Tool descriptions are read by the model — write them for an LLM audience, not a human reader.
- Tool inputs are JSON-schema'd via `jsonschema:"description=..."` tags on the input struct fields.
- Token-budget tool outputs aggressively (e.g., `chunkTokenBudget = 3000`). The model pays for every returned byte on every subsequent turn.
- Never put mandatory data in tools — if the model MUST see it, put it in the context block.

---

## Step 6: JSON parser (`scanner.go` + `scanner_test.go`) — use this for every flow

`JSONStringScanner` from `src/genkit/flows/post_assistant/scanner.go` is the canonical JSON parser for Anthropic structured output in this codebase. **Use it for every flow that consumes JSON from the model**, even non-streaming ones — it replaces `json.Unmarshal` and removes a whole class of production bugs.

Copy the file (`scanner.go` + `scanner_test.go`) verbatim into your flow package. The two `_test.go` groups travel with it: `TestScanner_*` covers streaming-delta behaviour and `TestValues_*` covers the drift patterns below.

### Why not `json.Unmarshal`?

Claude's JSON output drifts in production. We hit at least two distinct failures within hours of rolling out the obvious "ask for JSON, parse with `json.Unmarshal`" approach. The drift comes in many shapes; this table is the inventory we built up across post_assistant:

| Drift pattern | Example | `json.Unmarshal` | Scanner via `Values()` |
|---|---|---|---|
| Trailing comma | `{"a":"b",}` | ❌ hard fail | ✅ structural commas ignored at separator boundaries |
| Preamble / postamble prose | `Sure: {...}. Let me know!` | ❌ | ✅ `stPreamble` skips before `{`, `stDone` after `}` |
| Markdown code fence | ` ```json\n{...}\n``` ` | ❌ | ✅ same as above |
| Missing comma between string pairs | `{"a":"b" "c":"d"}` | ❌ | ✅ `stAfterValue` treats `"` as next-key start |
| Missing comma after bool/number | `{"saveVersion":true "note":"x"}` | ❌ | ✅ `stCollectLiteral` recovery |
| Literal newline inside string value | `{"x":"line1<LF>line2"}` (raw `\x0a`, not `\\n`) | ❌ | ✅ non-structural bytes are content |
| Unescaped tab / CR inside string | `"foo\x09bar"` | ❌ | ✅ same |
| Smart / curly quotes around strings | `{"a":"text"}` (`U+201C`/`U+201D`) | ❌ | partial — string treated as literal, raw fallback |
| Truncation mid-string | `{"a":"hel` (max_tokens hit) | ❌ | ✅ partial value returned |
| Truncation mid-literal | `{"saveVersion":tru` | ❌ | raw `"tru"` returned — caller treats as missing |
| Duplicate keys | `{"a":"first","a":"second"}` | last-wins | last-wins (same) |
| Nested objects | `{"meta":{...},"a":"b"}` | parsed | top-level only — nested values are absent from `Values()` |

The scanner's tolerance comes from walking the grammar character-by-character with explicit recovery states, not from regex post-processing. We tried the "tolerant pre-processor" approach (`stripTrailingCommas` + `extractJSONObject` + `insertMissingCommas` + `json.Unmarshal`) — it covered three patterns at the cost of fragile regex-on-JSON, and missed several others. The scanner is one piece of code that handles every row above; adding the next pattern (when one inevitably appears) is a state-machine tweak, not another regex.

### Why not `ai.WithOutputType(<Response>{})`?

Genkit's `WithOutputType` enables Anthropic's native structured output **and** a post-generation strict schema validator on the genkit side. The validator returns `(nil, err)` and discards the entire response on any blemish — meaning a single trailing comma takes down the whole turn, and we can't recover the raw text to repair it. The structured-output _hint_ (the schema attached to the request) does nudge the model, but in practice Claude 4.x with a clear "respond ONLY with the JSON object" prompt + scanner-based extraction is sufficient and dramatically more resilient.

If you observe systematic prose-drift in the wild (the model returning narrative instead of JSON for a specific flow), revisit the trade-off — but reach for prompt strengthening before reintroducing `WithOutputType`.

### Scanner API

```go
// watched determines which keys fire onDelta during streaming. Pass nil
// for both args if the flow doesn't stream — Values() still works.
scanner := NewJSONStringScanner(
    []string{"explanation", "updatedContent"},
    func(key, delta string) {
        switch key {
        case "explanation":
            emit(onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
        case "updatedContent":
            emit(onEvent, SSEEventContentDelta, DeltaEventPayload{Delta: delta})
        }
    },
)

// Feed every text chunk inside the WithStreaming callback.
scanner.Push(part.Text)

// After genkit.Generate returns, snapshot the parsed values.
vals := scanner.Values()

// Optional: the full raw text, for debugging.
raw := scanner.FullText()
```

`Values()` returns `map[string]any` with these typed values:

| Stored kind | Returned Go type | Notes |
|---|---|---|
| string literal | `string` | JSON escapes resolved (`\n`, `\uXXXX`, surrogate pairs) |
| `true` / `false` | `bool` | |
| `null` | `nil` | Map key still present |
| numeric literal | `float64` | `strconv.ParseFloat` — covers ints and floats |
| unparseable literal | `string` (raw trimmed) | E.g. truncated `"tru"` — caller decides if missing |
| nested object / array | absent from map | We don't extract these — keys are silently skipped |
| key never seen | absent from map | E.g. when the model omits an optional field |

### Sanity-check the extraction (graceful degradation, not strict failure)

The scanner returns whatever it saw — strings decoded, missing fields absent. The naive temptation is to require **every** field and fail loudly when one is missing. Don't: that fails legitimate runs that hit `max_tokens` mid-content.

Concretely: in our prompt schema the order is `explanation` → `updatedContent` → `action` → `saveVersion` → `versionNote`. When the model runs out of tokens during the long `updatedContent` field, every metadata field after it is missing. The user has already seen the streamed content; failing the call now would discard it and surface an unhelpful "didn't contain expected fields" error.

Instead, fail only when the response is **genuinely** unusable, and infer or default the rest. Three recovery cases sit before the strict-fail branch — pure-prose, truncated metadata, and missing explanation:

```go
// Pure-prose recovery: the model ignored the JSON envelope entirely
// and answered in plain text (often happens when the user asks a
// purely informational question). Salvage the raw text as the
// explanation of a "declined" response so the user at least sees
// the answer.
if result.Explanation == "" && result.LongField == "" {
    raw := strings.TrimSpace(scanner.FullText())
    if raw != "" && !strings.Contains(raw, "{") {
        log.Printf("<flow>[%s]: model emitted prose-only response (len=%d) — treating as informational/declined", req.<ID>, len(raw))
        result.Explanation = raw
        result.Action = "declined"
    }
}

switch {
case result.PrimaryField == "" && result.LongField == "":
    // Both empty AND the prose-recovery above didn't fill them.
    // The model produced nothing recognisable. Fail.
    log.Printf("<flow>[%s]: scanner found no usable fields (len=%d): %.500s",
        req.<ID>, len(scanner.FullText()), scanner.FullText())
    return nil, &AIError{Msg: "model response did not contain the expected fields"}

case result.Action == "" && result.LongField != "":
    // Truncation recovery: the model wouldn't have emitted the long
    // field if it had decided to "decline". Infer the action.
    log.Printf("<flow>[%s]: action missing — inferring 'edited' from non-empty long field (likely max_tokens truncation)", req.<ID>)
    result.Action = "edited"
}

// Surface a generic placeholder so the chat bubble isn't blank.
if result.Explanation == "" && result.LongField != "" {
    result.Explanation = "<sensible default e.g. 'Updated content.'>"
}
```

The pattern: identify the **one or two pieces** of the response that, if both empty, mean the model produced gibberish. Anything past that is recoverable through inference (action) or a placeholder (explanation). Always log the raw text on the genuine-fail branch so new drift shapes are diagnosable from server logs.

This also constrains how you order fields in your prompt schema: put the **shortest required fields** (action, flags) **before** the long ones (content, explanations). Field order in the prompt drives emission order; if action comes after a 5000-char content blob and `max_tokens` runs out, action never makes it. We deliberately violate this rule for `post_assistant` because user-facing streaming UX wants the explanation to arrive first — but that's why we need the truncation-recovery fallback above. If your flow doesn't have streaming UX constraints, prefer schema order: `action`, `saveVersion`, `versionNote`, `explanation`, `<long field last>`.

### What still needs your attention per flow

The scanner is verbatim-portable, but two things are flow-specific:

1. **`watched` list** — which keys do you want streaming-delta callbacks for. Typically the user-visible long-text fields (an `explanation`, a `body`, a `commentary`).
2. **The unpacking from `Values()` into your `<FlowName>Response`** — type-asserted per field, with a sanity check on required fields.

Everything else (state machine, escape decoding, UTF-8 carry, recovery states) is the same across every flow and shouldn't need touching.

---

## Step 7: Run function (`run.go`)

The heart of the flow. This template is the per-field-streaming-with-tools case — trim for simpler flows.

> **Variant — high-volume output:** if the flow produces an array of many independent items (50+, 100+) and the total output may exceed Claude's per-call cap (Sonnet 4.5 = 64K), the canonical "one Generate call per request" shape will hit `FinishReason == FinishReasonLength` in production. Skip ahead to the **Variant: parallel batching** section below — it replaces the model-call portion of this step with a deterministic slot allocator + parallel fan-out, but keeps the surrounding scaffolding (input validation, context assembly, scanner-based parsing, response shape) identical.
>
> **Variant — per-post persistence:** if the flow streams an array of records that you want to land in the database as they arrive (so a client disconnect mid-stream doesn't discard work, and a batch's hard failure doesn't roll back successful peers), see the **Variant: per-post persistence** section below. It absorbs the legacy "validate then persist at the end" pair of steps into the streaming callback — each parsed record is inline-validated, inline-inserted, and emitted with its new row's ID before the next chunk arrives. Layers cleanly with parallel batching.

```go
package <flow_name>

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "strings"
    "sync"
    "text/template"
    "time"

    "github.com/anthropics/anthropic-sdk-go"
    "github.com/firebase/genkit/go/ai"
    "github.com/firebase/genkit/go/genkit"
)

func run<FlowName>(
    ctx context.Context,
    g *genkit.Genkit,
    req <FlowName>Request,
    cfg <FlowName>FlowConfig,
    repos <FlowName>Repos,
    systemTmpl, contextTmpl *template.Template,
    tools *toolSet, // omit if no tools
    onEvent OnEventFunc,
) (*<FlowName>Response, error) {
    start := time.Now()

    // ── Validate input + load resource ──────────────────────────────────────
    if req.<Field> == "" {
        return nil, &ValidationError{Msg: "<field> is required"}
    }
    // repos.X.GetByID(ctx, id) — wrap sql.ErrNoRows as ValidationError

    // ── Assemble context + independent I/O in parallel ──────────────────────
    var actx *assistantContext
    var ctxErr error
    var history []*ai.Message
    var histErr error

    var wg sync.WaitGroup
    wg.Add(2)
    go func() {
        defer wg.Done()
        actx, ctxErr = assembleContextCached(ctx, resource, repos, systemTmpl, contextTmpl)
    }()
    go func() {
        defer wg.Done()
        // load history if conversational; otherwise delete this goroutine
    }()
    wg.Wait()

    if ctxErr != nil {
        return nil, fmt.Errorf("assemble context: %w", ctxErr)
    }
    if histErr != nil {
        return nil, fmt.Errorf("load history: %w", histErr)
    }

    // ── Inject per-request state for tools ──────────────────────────────────
    ctx = withRequestState(ctx, &requestState{ /* ... */ })

    // ── Call model ──────────────────────────────────────────────────────────
    maxTokens := cfg.MaxOutputTokens
    if maxTokens == 0 { maxTokens = 64000 } // see Step 2 FlowConfig comment
    maxTurns := cfg.MaxTurns
    if maxTurns == 0 { maxTurns = 8 }       // see Step 2 FlowConfig comment
    mc := modelconfig.Resolve(ctx, modelconfig.Flow<Name>, modelconfig.SlotMain)
    systemBlock := actx.SystemPrompt + "\n\n" + actx.ContextBlock

    // Scanner + tool dedup state live outside the callback so they survive
    // across tool-use round-trips within the same Generate call.
    scanner := NewJSONStringScanner(
        []string{"explanation", "updatedContent"},
        func(key, delta string) {
            switch key {
            case "explanation":
                emit(onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
            case "updatedContent":
                emit(onEvent, SSEEventContentDelta, DeltaEventPayload{Delta: delta})
            }
        },
    )
    emittedToolCalls := map[string]bool{}
    emittedToolResults := map[string]bool{}

    streamCb := func(_ context.Context, chunk *ai.ModelResponseChunk) error {
        if chunk == nil || chunk.Aggregated {
            return nil
        }
        for _, part := range chunk.Content {
            switch {
            case part.IsText():
                scanner.Push(part.Text)
            case part.IsToolRequest():
                tr := part.ToolRequest
                if tr == nil || tr.Partial || emittedToolCalls[tr.Ref] {
                    continue
                }
                emittedToolCalls[tr.Ref] = true
                emit(onEvent, SSEEventToolCall, ToolCallEventPayload{
                    Name: tr.Name, Input: tr.Input, Ref: tr.Ref,
                })
            case part.IsToolResponse():
                tr := part.ToolResponse
                if tr == nil || emittedToolResults[tr.Ref] {
                    continue
                }
                emittedToolResults[tr.Ref] = true
                emit(onEvent, SSEEventToolResult, ToolResultEventPayload{
                    Name: tr.Name, Ref: tr.Ref, OK: true,
                })
            }
        }
        return nil
    }

    resp, err := genkit.Generate(ctx, g,
        ai.WithModelName(mc.Ref),
        ai.WithSystem(systemBlock),
        ai.WithMessages(history...),              // omit if not conversational
        ai.WithPrompt(req.<Prompt>),
        ai.WithTools(tools.listAssets /* , ... */), // omit if no tools
        ai.WithMaxTurns(maxTurns),                // see Step 2 FlowConfig + gotcha #12
        ai.WithStreaming(streamCb),
        // See Step 8b: CallMiddleware handles refusals and cache-write metering;
        // CallConfig carries MaxTokens, clamped to mc.Model's max output.
        ai.WithMiddleware(cfg.Provider.CallMiddleware(modelconfig.Flow<Name>, usage.Record)),
        cfg.Provider.CallConfig(mc.Model, maxTokens),
        // NB: do NOT add ai.WithOutputType — see Step 6 "Why not ai.WithOutputType"
    )
    if err != nil {
        return nil, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
    }

    // Deterministic truncation signal: when Anthropic stops at max_tokens,
    // genkit surfaces it as FinishReasonLength. Log loudly so the cap can
    // be tuned (env MAX_OUTPUT_TOKENS) before users see the recovery
    // branches in the assemble block kick in.
    if resp.FinishReason == ai.FinishReasonLength {
        var outputTokens int64
        if resp.Usage != nil {
            outputTokens = int64(resp.Usage.OutputTokens)
        }
        log.Printf("<flow>[%s]: TRUNCATED — finish_reason=length, output_tokens=%d, cap=%d. Bump MAX_OUTPUT_TOKENS or shorten the input.",
            req.<ID>, outputTokens, maxTokens)
    }

    // ── Assemble response from scanner ──────────────────────────────────────
    // The scanner has been processing every chunk in the streaming callback
    // above. Values() returns the parsed top-level fields without going
    // through encoding/json — see Step 6 for the drift patterns this fixes.
    vals := scanner.Values()
    result := <FlowName>Response{}
    if s, ok := vals["explanation"].(string); ok    { result.Explanation = s }
    if s, ok := vals["updatedContent"].(string); ok { result.UpdatedContent = s }
    if s, ok := vals["action"].(string); ok         { result.Action = s }
    if b, ok := vals["saveVersion"].(bool); ok      { result.SaveVersion = b }
    if s, ok := vals["versionNote"].(string); ok    { result.VersionNote = s }

    // Graceful degradation against truncated responses (see Step 6's
    // "Sanity-check the extraction" for the rationale). Fail only when the
    // response is genuinely unusable; recover the rest through inference.
    switch {
    case result.Explanation == "" && result.UpdatedContent == "":
        raw := scanner.FullText()
        log.Printf("<flow>[%s]: scanner found no usable fields (len=%d): %.500s",
            req.<ID>, len(raw), raw)
        return nil, &AIError{Msg: "model response did not contain the expected fields"}

    case result.Action == "" && result.UpdatedContent != "":
        log.Printf("<flow>[%s]: action missing — inferring 'edited' from non-empty content (likely max_tokens truncation)", req.<ID>)
        result.Action = "edited"
    }
    if result.Explanation == "" && result.UpdatedContent != "" {
        result.Explanation = "Updated post content."
    }

    // ── Apply domain conversions + persist ──────────────────────────────────
    // e.g. Markdown → BlockNote JSON, create version snapshot, update resource
    // Persist both the user turn and the model turn if conversational.

    log.Printf("<flow_name>[%s]: done in %s", req.<ID>, time.Since(start).Round(time.Millisecond))

    // Canonical final event. Deltas before this were preview-only.
    emit(onEvent, SSEEventComplete, &result)

    return &result, nil
}
```

**Rules:**
- `genkit.Generate` + `WithStreaming` callback, NOT `genkit.GenerateStream`. The former preserves tool round-tripping naturally; the latter is just a wrapper that yields chunks via an iterator (less control).
- **No `ai.WithOutputType`.** It enables a strict post-generation validator that hard-fails on common Claude drift (trailing comma, etc.) and discards the response. See Step 6 for the rationale and the comprehensive drift table.
- **Use `scanner.Values()` instead of `json.Unmarshal`.** Same reason — full character-level tolerance, partial response support on truncation, and one less moving part. Sanity-check required fields after extraction with **graceful degradation** (see Step 6's "Sanity-check the extraction"): fail only when the response is genuinely unusable; recover missing metadata fields through inference and placeholders.
- Skip chunks where `Aggregated == true` — those are recap frames that would double-feed the scanner.
- Dedupe tool events by `Ref` with a local map. Emit tool_call only when `!Partial` (complete request).
- Log the full prompt once per call for debugging: `log.Printf("<flow>: system prompt:\n%s", systemPrompt)`. Do NOT log the response — it may contain user data. Exception: log raw text (truncated to ~500 chars) when the required-field sanity check trips, so new drift shapes are diagnosable.
- Wrap `sql.ErrNoRows` as `ValidationError{Msg: "<resource> not found"}`. Wrap model errors as `AIError`. Never return raw `errors.New` from the top level — the handler won't know the HTTP code.

---

## Step 8: Flow file (`flow.go`)

```go
package <flow_name>

import (
    "context"
    "embed"
    "fmt"
    "text/template"

    "github.com/firebase/genkit/go/core"
    "github.com/firebase/genkit/go/genkit"
)

//go:embed prompts/<flow_name>.tmpl
var promptFS embed.FS

var <FlowName>Flow *core.Flow[<FlowName>Request, *<FlowName>Response, struct{}]

// runner bypasses the Genkit flow wrapper so we can thread OnEventFunc
// through for SSE streaming. The wrapped Flow is for the Dev UI only.
var <flowName>Runner func(ctx context.Context, req <FlowName>Request, onEvent OnEventFunc) (*<FlowName>Response, error)

func Init<FlowName>(g *genkit.Genkit, cfg <FlowName>FlowConfig, repos <FlowName>Repos) error {
    raw, err := promptFS.ReadFile("prompts/<flow_name>.tmpl")
    if err != nil {
        return fmt.Errorf("load <flow_name>.tmpl: %w", err)
    }
    tmpl, err := template.New("<flow_name>").Parse(string(raw))
    if err != nil {
        return fmt.Errorf("parse <flow_name>.tmpl: %w", err)
    }
    systemTmpl := tmpl.Lookup("system")
    contextTmpl := tmpl.Lookup("context")
    if systemTmpl == nil || contextTmpl == nil {
        return fmt.Errorf("<flow_name>.tmpl must define both system and context blocks")
    }

    tools := defineTools(g) // omit if no tools

    <FlowName>Flow = genkit.DefineFlow(g, "<flowName>",
        func(ctx context.Context, req <FlowName>Request) (*<FlowName>Response, error) {
            return run<FlowName>(ctx, g, req, cfg, repos, systemTmpl, contextTmpl, tools, nil)
        },
    )

    <flowName>Runner = func(ctx context.Context, req <FlowName>Request, onEvent OnEventFunc) (*<FlowName>Response, error) {
        return run<FlowName>(ctx, g, req, cfg, repos, systemTmpl, contextTmpl, tools, onEvent)
    }

    return nil
}

// New<FlowName>Callback returns the callback the HTTP handler will invoke.
// Pass a non-nil OnEventFunc for SSE streaming, nil for a silent call.
func New<FlowName>Callback() func(ctx context.Context, req <FlowName>Request, onEvent OnEventFunc) (*<FlowName>Response, error) {
    return func(ctx context.Context, req <FlowName>Request, onEvent OnEventFunc) (*<FlowName>Response, error) {
        return <flowName>Runner(ctx, req, onEvent)
    }
}

// emit is a no-op when onEvent is nil. Keeps run.go free of nil checks.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
    if onEvent != nil {
        onEvent(name, data)
    }
}
```

**Rules:**
- Two registration paths: `DefineFlow` (shows up in Genkit Dev UI for exploration, runs silently) and the direct runner closure (streams events).
- Package init via `InitX(g, cfg, repos) error` — called once from server.go after `genkit.Init`.
- Never make the callback receiver a struct — a closure returned from `NewXCallback()` is simpler and harder to misuse.

---

## Step 8b: Usage metering & enforcement (CON-86) — every model-calling flow needs this

Every flow that calls a model is metered and gated by the CON-86 usage layer.
Three **nil-safe** dependencies thread through `FlowConfig`. They are all nil
when `ANALYTICS_DSN` is empty (analytics disabled) — so guard nothing, just call
the methods; nil receivers are no-ops. Miss the wiring and the flow silently
records nothing / never enforces (no error, no signal).

### FlowConfig fields (`flow.go`)

Add alongside `MaxOutputTokens`, and import
`"github.com/ogen-app/ogen/src/infra/vendors/llm"` + `"github.com/ogen-app/ogen/src/kernel/usage"`:

```go
// Provider builds the call config and middleware, so the flow never imports
// the Anthropic SDK (CON-86 FR12).
Provider *llm.Provider
// Recorder captures usage events async; nil disables recording (FR5/FR10).
Recorder *usage.Recorder
// Checker gates the flow against the tenant's spend caps; nil = no gate.
Checker *usage.Checker
```

There is no model id in config or env. Models are assigned per flow slot (and
per tier) in Harbor.

### Register the flow's slot, resolve it once per call

Add the flow and its slot(s) to the catalog in
`src/domain/modelconfig/catalog.go`, and a default in
`modelconfig.SeedDefaults` (it seeds the slot's row on first boot). Then
resolve once per call with `modelconfig.Resolve(ctx, flow, slot)`, and use that
one `Resolved` for `mc.Ref`, `CallConfig(mc.Model, …)` and usage recording.

### Enforcement gate — before the first paid call

Call `Enforce` once, AFTER any cache short-circuit (never block a cached,
provider-free result) and BEFORE the first model call:

```go
// Enforcement gate (CON-86 FR9). Nil checker = no gate.
if err := cfg.Checker.Enforce(ctx); err != nil {
    return nil, err // *usage.LimitExceededError when blocked
}
```

`Enforce` is nil-safe and **fails open** (analytics read errors → allow). It
returns `*usage.LimitExceededError` (embeds `Period`, `CapMicros`, `SpentMicros`,
`Mode`) only when the tenant is genuinely over an *enforce*-mode cap; `warn`
mode never blocks.

### Model call goes through the Provider

The model is never hardcoded or read from env: resolve the flow's slot (Harbor
assigns it per tier) and pass the resolved model to the Provider:

```go
mc := modelconfig.Resolve(ctx, modelconfig.Flow<Name>, modelconfig.SlotMain)
u := flowkit.Usage{Recorder: cfg.Recorder, Model: mc, Feature: "<feature>", Component: logComponent}
...
out, resp, err := genkit.GenerateData[T](ctx, g,
    ai.WithModelName(mc.Ref),
    ai.WithSystem("%s", prompts.system),
    ai.WithPrompt("%s", userPrompt),
    ai.WithMiddleware(cfg.Provider.CallMiddleware(modelconfig.Flow<Name>, u.Record)),
    cfg.Provider.CallConfig(mc.Model, maxTokens), // MaxTokens, clamped to the model's max output
)
```

`CallConfig` returns an `ai.WithConfig(...)` option — genkit **rejects two
`WithConfig`s**, so pass exactly one `CallConfig` per call and no other config
option. Its cap covers thinking too (Claude 5.x thinks by default).

`CallMiddleware` turns a refusal into `llm.ErrRefused` (recording the refused
call through `u.Record`) and meters prompt-cache writes. genkit also allows only
one `WithMiddleware` per call, so list any other middleware in the same call,
before it: `ai.WithMiddleware(u.Meter(), cfg.Provider.CallMiddleware(...))`. A
tool loop that resends the same prefix every round adds `llm.CachePrompt()`.

### Record usage — after each completed call

After every successful generation (including a retry that still consumed
tokens), record it. `RecordResp` runs the vendor's meter over the genkit
`*ai.ModelResponse` (the second return of `genkit.Generate*`):

```go
// One event per completed call (CON-86 FR1). Nil recorder = no-op.
u.Record(ctx, resp)
```

- `"<feature>"` is a stable slug for this flow (e.g. `"content_plan"`,
  `"post_quality"`) — it becomes the `feature` dimension in `usage_events` and
  the summary breakdown. Pick one, use it everywhere in the flow.
- In batched/multi-call flows, call `RecordResp` **inside the per-call loop** so
  each model call is one event.

### Tenant context is mandatory

Recorder and Checker read the tenant from `ctx` (`tenantctx.From`). An
untenanted call records nothing (the Recorder skips it) and cannot enforce.
Run the flow in a tenant context — the same scoping every repo relies on:
- Request-driven handler: pass `c.Context()` (the auth middleware sets
  `c.Locals(tenantctx.Key, session.TenantID)`, which `c.Context().Value` reads).
- Background goroutine: rebuild it — `tenantctx.With(context.Background(), session.TenantID)`.
If your repo reads work in the flow, the meter works.

### HTTP handler — map the block to 402

The enforcement error must surface as **402 Payment Required**, not a 500. Add a
case to the handler's error switch (SSE `code`/`msg` or JSON), alongside the
existing `ValidationError`/`AIError` cases:

```go
var le *usage.LimitExceededError
switch {
case errors.As(err, &le):
    code = fiber.StatusPaymentRequired // 402
    msg = le.Error()
case errors.As(err, &ve): // ...existing ValidationError → 400
    ...
}
```

> ⚠️ The existing flows (content_plan, post_assistant, post_quality,
> enrich_brief) do **not** yet add this case, so a blocked tenant currently falls
> through to a 500 — don't copy their handler error switch verbatim. New flows
> should add the 402 case; retrofitting the four old handlers is a known
> follow-up.

---

## Step 9: Server wiring (`src/transport/server/<flow_name>.go` + edits to `src/transport/server/server.go`)

Small adapter file to keep server.go clean:

```go
// src/transport/server/<flow_name>.go
package server

import (
    "context"
    "fmt"

    "github.com/firebase/genkit/go/ai"
    "github.com/firebase/genkit/go/genkit"

    "github.com/ogen-app/ogen/src/kernel/config"
    "github.com/ogen-app/ogen/src/genkit/flows/<flow_name>"
    "github.com/ogen-app/ogen/src/kernel/usage"
    "github.com/ogen-app/ogen/src/infra/vendors/llm"
)

func init<FlowName>(
    g *genkit.Genkit,
    cfg *config.Config,
    provider *llm.Provider,   // CON-86 — see Step 8b
    recorder *usage.Recorder, // nil when analytics disabled (nil-safe)
    checker *usage.Checker,   // nil when analytics disabled (nil-safe)
    embedder ai.Embedder,
    repos <flow_name>.<FlowName>Repos,
) (func(ctx context.Context, req <flow_name>.<FlowName>Request, onEvent <flow_name>.OnEventFunc) (*<flow_name>.<FlowName>Response, error), error) {
    flowCfg := <flow_name>.<FlowName>FlowConfig{
        Provider:        provider,
        Recorder:        recorder,
        Checker:         checker,
        MaxOutputTokens: cfg.MaxOutputTokens,
        Embedder:        embedder,
    }
    if err := <flow_name>.Init<FlowName>(g, flowCfg, repos); err != nil {
        return nil, fmt.Errorf("init <flow_name> flow: %w", err)
    }
    return <flow_name>.New<FlowName>Callback(), nil
}
```

In `src/transport/server/server.go`, add the callback var declaration near the other callbacks and register only if the Anthropic key is configured:

```go
var <flowName>Callback func(context.Context, <flow_name>.<FlowName>Request, <flow_name>.OnEventFunc) (*<flow_name>.<FlowName>Response, error)

// ... later, inside the Anthropic-key block. provider is already built there
// (provider := llm.NewProvider()); recorder and
// checker come from initUsage's usageWiring (usageWiring.recorder/.checker) —
// both nil-safe when analytics is disabled:
<flowName>Repos := <flow_name>.<FlowName>Repos{ /* ... */ }
<flowName>Callback, err = init<FlowName>(g, cfg, provider, usageWiring.recorder, usageWiring.checker, embedder, <flowName>Repos)
if err != nil {
    return nil, err
}

// ... at handler registration:
handlers.New<Resource>Handler(/* existing args */, <flowName>Callback).Register(app)
```

**Rules:**
- Gate registration behind `cfg.AnthropicAPIKey != ""`. Leaves the callback nil → handler emits 503.
- Use ONE `genkit.Init(ctx, genkit.WithPlugins(plugins...))` for all plugins — see `CON-42: Consolidate Genkit into single instance` in git log for why.
- Use the native `plugins/anthropic` import path, NOT `plugins/compat_oai/anthropic`. The compat plugin rejects `anthropic.MessageNewParams` as an unknown config type.

---

## Step 10: HTTP handler (`src/transport/handlers/<resource>.go`)

Two shapes: JSON (non-streaming) or SSE (streaming). Use SSE if the flow emits events.

### SSE handler template

```go
// imports you'll need:
//   "bufio"
//   "context"
//   "encoding/json"
//   "errors"
//   "fmt"
//   "github.com/valyala/fasthttp"
//   "github.com/ogen-app/ogen/src/genkit/flows/<flow_name>"

// <Verb> godoc
// @Summary      <Resource> <verb> (SSE)
// @Description  Streams progress via Server-Sent Events.
// @Description  Events: <list of event names> carry <shape>.
// @Description  "complete" carries the final <FlowName>Response.
// @Description  "error" carries {"message":"...","code":<http_code>}.
// @Tags         <resource>
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id    path      string           true  "<Resource> Sqid"
// @Param        body  body      <verbRequest>    true  "Payload"
// @Success      200  "SSE stream"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/<resource>/{id}/<verb> [post]
func (h *<Resource>Handler) <Verb>(c *fiber.Ctx) error {
    if h.<flowName> == nil {
        return fiber.NewError(fiber.StatusServiceUnavailable, "<flow> is not available")
    }
    var req <verb>Request
    if err := c.BodyParser(&req); err != nil {
        return fiber.NewError(fiber.StatusBadRequest, err.Error())
    }
    if err := validate.Struct(&req); err != nil {
        return fiber.NewError(fiber.StatusBadRequest, validationError(err).Error())
    }

    c.Set("Content-Type", "text/event-stream")
    c.Set("Cache-Control", "no-cache")
    c.Set("Connection", "keep-alive")
    c.Set("X-Accel-Buffering", "no")

    id := c.Params("id")
    payload := req // capture before the stream writer starts
    runner := h.<flowName>

    c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
        writeEvent := func(event string, data any) {
            b, _ := json.Marshal(data)
            fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
            _ = w.Flush()
        }

        onEvent := <flow_name>.OnEventFunc(func(name <flow_name>.SSEEventKind, data any) {
            writeEvent(string(name), data)
        })

        // NB: use context.Background() here — the fiber ctx is cancelled
        // the moment the handler returns, which happens before the stream
        // writer runs.
        _, err := runner(context.Background(), <flow_name>.<FlowName>Request{
            <ID>: id,
            <Field>: payload.<Field>,
        }, onEvent)
        if err != nil {
            code := fiber.StatusInternalServerError
            msg := err.Error()
            var ve *<flow_name>.ValidationError
            var ae *<flow_name>.AIError
            switch {
            case errors.As(err, &ve):
                code = fiber.StatusBadRequest
                msg = ve.Msg
            case errors.As(err, &ae):
                code = fiber.StatusBadGateway
                msg = ae.Msg
            }
            writeEvent(string(<flow_name>.SSEEventError), <flow_name>.ErrorEventPayload{Message: msg, Code: code})
            return
        }
        // "complete" was emitted by the runner itself; nothing more to write.
    }))

    return nil
}
```

**Rules:**
- `context.Background()` inside the stream writer — the fiber request ctx is cancelled the moment the handler returns `nil`, which is BEFORE the stream writer runs.
- Capture `id` / `req` into local variables before starting the stream writer (fiber may reuse its buffers after return).
- Error mapping is `errors.As` against the typed errors from `types.go`. Add new error types there if you need a new HTTP code.
- `X-Accel-Buffering: no` tells nginx/upstream proxies to flush immediately.
- Thread the callback through the handler struct constructor (see content_plan + post_assistant for the pattern).

### JSON handler shape (non-streaming flow)

If the flow doesn't stream, the handler is trivial:

```go
resp, err := h.<flowName>(c.Context(), <flow_name>.<FlowName>Request{ /* ... */ }, nil /* onEvent */)
if err != nil {
    var ve *<flow_name>.ValidationError
    var ae *<flow_name>.AIError
    switch {
    case errors.As(err, &ve):
        return fiber.NewError(fiber.StatusBadRequest, ve.Msg)
    case errors.As(err, &ae):
        return fiber.NewError(fiber.StatusBadGateway, ae.Msg)
    }
    return err
}
return c.JSON(resp)
```

---

## Step 11: Tests (`src/transport/handlers/<resource>_test.go`)

Handler-layer only. A fresh, fully-migrated **Postgres** database via `mustOpenTestDBWithMigrations()` (backed by `src/pgtest`; `make test` provisions the Postgres instance). Stub the flow callback with a function that emits synthetic events.

```go
// inside the existing Describe block:
Context("with a stub <flowName> callback", func() {
    buildStubApp := func(
        stub func(context.Context, <flow_name>.<FlowName>Request, <flow_name>.OnEventFunc) (*<flow_name>.<FlowName>Response, error),
    ) (*fiber.App, *http.Cookie, string) {
        stubApp := fiber.New(fiber.Config{ /* usual error handler */ })
        // ... repos + auth + handlers
        handlers.New<Resource>Handler(/* ... */, stub).Register(stubApp)
        // ... seed user + login + create parent resource; return (app, authCookie, resourceID)
    }

    type sseEvent struct{ event, data string }
    parseSSE := func(body io.Reader) []sseEvent {
        var events []sseEvent
        scanner := bufio.NewScanner(body)
        var curEvent, curData string
        for scanner.Scan() {
            line := scanner.Text()
            switch {
            case strings.HasPrefix(line, "event: "):
                curEvent = strings.TrimPrefix(line, "event: ")
            case strings.HasPrefix(line, "data: "):
                curData = strings.TrimPrefix(line, "data: ")
            case line == "":
                if curEvent != "" { events = append(events, sseEvent{curEvent, curData}) }
                curEvent, curData = "", ""
            }
        }
        return events
    }

    It("streams the expected event sequence", func() {
        final := &<flow_name>.<FlowName>Response{ /* ... */ }
        stub := func(_ context.Context, _ <flow_name>.<FlowName>Request, onEvent <flow_name>.OnEventFunc) (*<flow_name>.<FlowName>Response, error) {
            onEvent(<flow_name>.SSEEvent<X>, <flow_name>.DeltaEventPayload{Delta: "..."})
            onEvent(<flow_name>.SSEEventComplete, final)
            return final, nil
        }
        stubApp, cookie, id := buildStubApp(stub)

        req := /* POST to /api/<resource>/<id>/<verb> */
        resp, _ := stubApp.Test(req, 5000)
        Expect(resp.StatusCode).To(Equal(200))
        Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

        events := parseSSE(resp.Body)
        names := make([]string, len(events))
        for i, e := range events { names[i] = e.event }
        Expect(names).To(Equal([]string{"<x>", "complete"}))
    })

    It("emits an error event on ValidationError", func() {
        stub := func(_ context.Context, _ <flow_name>.<FlowName>Request, _ <flow_name>.OnEventFunc) (*<flow_name>.<FlowName>Response, error) {
            return nil, &<flow_name>.ValidationError{Msg: "bad input"}
        }
        // ... POST, parse SSE, expect 1 event with event=="error" and data containing `"code":400`
    })
})
```

**Rules:**
- Never mock the DB. Use `mustOpenTestDBWithMigrations()` per the project's testing conventions.
- Tests at the handler layer only — don't test the flow's internal functions directly (the scanner is the one exception; unit tests for pure logic are fine).
- Integration tests for the real model live under `src/integration/` with the `//go:build integration` tag — they need `ANTHROPIC_API_KEY` and the native `plugins/anthropic` plugin.

---

## Step 12 (optional): Frontend wiring

If there's a React prototype, use a `ReadableStream` reader — `EventSource` doesn't support POST bodies.

```jsx
const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
    credentials: "include",
    body: JSON.stringify(payload),
});
if (!res.ok || !res.body) throw new Error(`Request failed (${res.status})`);

const reader = res.body.getReader();
const decoder = new TextDecoder();
let buf = "";

const flushFrames = () => {
    let idx;
    while ((idx = buf.indexOf("\n\n")) >= 0) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let event = "", dataStr = "";
        for (const line of frame.split("\n")) {
            if (line.startsWith("event: ")) event = line.slice(7);
            else if (line.startsWith("data: ")) dataStr = line.slice(6);
        }
        if (event) handleEvent(event, dataStr);
    }
};

while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    flushFrames();
}
buf += decoder.decode();
flushFrames();
```

**Patterns for per-field-delta streaming UIs:**

- Store deltas as an array of chunks, not a concatenated string. React's index keys preserve already-mounted spans so only newly-appended spans re-run a fade-in animation (existing chunks stay put). See `prototyping/CON-42-post-assistant/src/components/PostContent.jsx::StreamingPreview`.
- Render a spinner next to the assistant bubble driven by a `streaming: true` flag on the message, flipped to false on `complete` / `error` / fetch catch.
- Expose tool calls as chips with a `running` → `done` state machine keyed on tool `Ref`. See `src/components/ChatPanel.jsx` for the exact render.

---

## Variant: parallel batching for high-volume output

When the flow produces an array of independent items and the total output may exceed Claude's per-call cap (Sonnet 4.5 = 64K), generate in K-sized batches in parallel instead of one monolithic call. Canonical reference: `src/genkit/flows/content_plan/` after CON-67.

### When to reach for this

- Output is an array of independent items (posts, summaries, classifications) — items in slot N don't depend on items in slot N±1.
- The total count is known per request and may grow large (50+, 100+, …).
- Symptom in logs: `FinishReason == ai.FinishReasonLength` firing on real-world inputs.
- Or: total per-request latency matters more than minimising API calls.

### Architecture

Three pieces, plus prompt and config changes. The single-shot run function from Step 7 stays as a fallback for callers that don't know the item count up front.

1. **Slot allocator** (`batch.go`) — pure logic, no genkit deps. Plans N items across whatever domain dimensions matter (phases × platforms × dates × …). Chunks slots into K-sized batches with deterministic global indices.
2. **Parallel fan-out helper** (`runBatchesParallel`) — extracted from `runX`. Takes a `gen(ctx, spec, emit)` callback so the test layer can stub it. Spawns goroutines (semaphore-capped), serialises emit through a mutex, aggregates results in batch order with partial-success semantics.
3. **Per-batch prompt rendering** — extend the template data struct with an optional `Batch *batchSpec` field; the user prompt template emits a slot table (count + per-dimension breakdowns + scope window) when `.Batch` is non-nil and falls through to the unconstrained "produce N items" form when nil.

### Slot allocator (`batch.go`)

Pure logic, easily unit-tested. Each batch's `GlobalStartIndex` is the slot index of its first item in the campaign-wide ordering — used downstream to stamp every emitted SSE event with a stable global index.

```go
type batchSpec struct {
    Index            int
    GlobalStartIndex int
    PostCount        int
    PhaseCounts      []phaseCount   // your domain's primary dimension
    PlatformCounts   []platformCount
    DateWindow       dateWindow     // if your domain is date-bounded
}

func planBatches(
    totalItems int,
    phases []resolvedPhase,
    platforms []resolvedPlatform,
    start, end time.Time,
    maxPerBatch int,
) []batchSpec {
    if totalItems <= 0 || len(phases) == 0 || len(platforms) == 0 {
        return nil  // caller falls back to single-shot
    }
    if maxPerBatch <= 0 {
        maxPerBatch = totalItems
    }
    // 1. evenSplit(totalItems, len(phases))     — front-load remainder to earliest
    // 2. computeDateWindows(start, end, len(phases))
    // 3. for each phase: evenSplit(phasePosts[p], len(platforms))
    // 4. flatten into ordered slot list (phase ASC by Sequence → platform in declared order)
    // 5. chunk slots into batches of maxPerBatch; aggregate per-batch counts + window union
    // ... see content_plan/batch.go for the canonical implementation
}
```

**Rules for the allocator:**
- Defensive-sort dimensions by their canonical ordering field (e.g. `Sequence`) before splitting — repository queries don't always return them ordered, and front-loading depends on order.
- `evenSplit(total, n)` distributes total into n buckets, remainder to earliest. Trivially testable.
- Skip dimensions that get 0 items (don't emit empty `phaseCount` rows in the prompt — the model treats them as constraint noise).
- Date-window splitter: if `dayCount < numPhases`, collapse all phases to the full range rather than emitting zero-day windows that produce invalid publishDates downstream.

### Parallel fan-out helper

```go
func runBatchesParallel(
    ctx context.Context,
    batches []batchSpec,
    maxParallel int,
    gen func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]Item, error),
    onEvent OnEventFunc,
) ([]Item, []string, error) {
    if maxParallel <= 0 {
        maxParallel = 1
    }

    // SSE writer is single-writer — serialise emit before fanning out.
    // Without the lock the bytes interleave across goroutines and frame
    // parsing on the client breaks; symptoms are intermittent, not clean.
    var emitMu sync.Mutex
    safeEmit := onEvent
    if onEvent != nil {
        safeEmit = func(name SSEEventKind, payload any) {
            emitMu.Lock()
            defer emitMu.Unlock()
            onEvent(name, payload)
        }
    }

    type result struct {
        items []Item
        err   error
    }
    results := make([]result, len(batches))
    sem := make(chan struct{}, maxParallel)
    var wg sync.WaitGroup

    for i := range batches {
        i, spec := i, batches[i]
        wg.Add(1)
        sem <- struct{}{}
        go func() {
            defer wg.Done()
            defer func() { <-sem }()
            items, err := gen(ctx, spec, safeEmit)
            results[i] = result{items: items, err: err}
        }()
    }
    wg.Wait()

    // Aggregate in batch order, not completion order — the persisted set
    // and any future replay both need deterministic slot ordering.
    var all []Item
    var warnings []string
    failures := 0
    for i, r := range results {
        if r.err != nil {
            failures++
            warnings = append(warnings, fmt.Sprintf(
                "batch %d/%d (slots %d-%d) failed: %v",
                i+1, len(batches),
                batches[i].GlobalStartIndex,
                batches[i].GlobalStartIndex+batches[i].PostCount-1,
                r.err,
            ))
            continue
        }
        all = append(all, r.items...)
    }
    // Hard error only when EVERY batch failed — anything else is partial
    // success and the surviving items are returned with warnings.
    if failures == len(batches) {
        return nil, warnings, &AIError{Msg: fmt.Sprintf("all %d batches failed; first error: %v", len(batches), results[0].err)}
    }
    return all, warnings, nil
}
```

### Calling pattern in `runX`

```go
batches := planBatches(estCount, phases, platforms, *start, *end, cfg.MaxPostsPerBatch)
if len(batches) == 0 {
    // Single-shot fallback — preserve the pre-batching behaviour for
    // callers that don't know the count up front. Same code path as Step 7.
    userPrompt, _ := renderTemplate(cfg.userTmpl, data)
    return generateItemsStreaming(ctx, g, modelName, systemPrompt, userPrompt, maxTokens, 0, onEvent)
}

// System prompt is identical for every batch — render once. Anthropic's
// prompt cache hits across batches when the system block is byte-identical.
systemPrompt, _ := renderTemplate(cfg.systemTmpl, data)

gen := func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]Item, error) {
    batchData := data
    batchData.Batch = &spec
    userPrompt, err := renderTemplate(cfg.userTmpl, batchData)
    if err != nil {
        return nil, fmt.Errorf("render user prompt for batch %d: %w", spec.Index, err)
    }
    return generateItemsStreaming(ctx, g, modelName, systemPrompt, userPrompt, maxTokens, spec.GlobalStartIndex, emit)
}
items, warnings, err := runBatchesParallel(ctx, batches, cfg.MaxParallelBatches, gen, onEvent)
```

### Per-item global indices

`generateItemsStreaming` (your per-batch streaming helper) takes an extra `globalStartIndex int` param and stamps it onto every emitted event:

```go
emit(onEvent, SSEEventItem, ItemEventPayload{
    Item:  item,
    Index: globalStartIndex + len(items),  // stable global slot ID
})
```

Under parallel batching, posts arrive **interleaved by completion time, not slot index**. The `Index` field becomes a stable identifier the UI uses to place items in deterministic slot order rather than appending in arrival order. Update the doc comment on `<Item>EventPayload.Index` to say so explicitly — the React layer relies on this contract.

### Prompt template variant

Two changes:

1. **System prompt** — drop "Generate exactly N items" (it's now per-batch); replace with "exactly the number the user message asks for" and add "honour any per-dimension constraints stated in the user message — distributions are pre-computed and not negotiable".
2. **User prompt** — wrap the per-batch slot table in `{{if .Batch}}`; keep the legacy `{{else if gt .EstimatedItemCount 0}}` arm so the single-shot fallback still works.

```go-template
{{define "user"}}
... global request context ...

{{- if .Batch}}
This request is one slice of a larger plan. Generate exactly {{.Batch.PostCount}} items in this response — the JSON array must contain exactly {{.Batch.PostCount}} objects, no more, no fewer.

Within this batch, distribute items as follows (counts are non-negotiable):
{{- range .Batch.PhaseCounts}}
  • Phase {{.Sequence}} "{{.PhaseName}}" (id: "{{.PhaseID}}"): {{.Count}}
{{- end}}

Platform mix for this batch:
{{- range .Batch.PlatformCounts}}
  • {{.PlatformName}} (id: "{{.PlatformID}}"): {{.Count}}
{{- end}}

Restrict every <date/scope> to the window {{.Batch.DateWindow.Start}} — {{.Batch.DateWindow.End}} (inclusive).
{{- else if gt .EstimatedItemCount 0}}
Required count: exactly {{.EstimatedItemCount}} items.
{{- end}}

... static reference material ...
{{end}}
```

### Configuration knobs

```go
// in <FlowName>FlowConfig
MaxPostsPerBatch   int  // 0 → 30 (sized for 64K output / ~800 tok/post + headroom)
MaxParallelBatches int  // 0 → 5 (tier-1 Anthropic ITPM safety knob)
```

Plumb through `src/kernel/config/config.go` (env vars `MAX_POSTS_PER_BATCH`, `MAX_PARALLEL_BATCHES`) and `src/transport/server/<flow_name>.go`. Defaults assume Sonnet 4.5 + tier-1 limits — bump for Haiku (more parallelism affordable) or tier 2+ accounts.

### Testing

The extracted `runBatchesParallel` helper is the unit-testable surface. Production hits Anthropic; tests pass a stub `gen` that simulates timing, partial failures, and emit concurrency without the network.

```go
// 1. Aggregation in batch order despite staggered completion
delays := []time.Duration{30 * time.Millisecond, 5 * time.Millisecond, 60 * time.Millisecond}
gen := func(_ context.Context, spec batchSpec, _ OnEventFunc) ([]Item, error) {
    time.Sleep(delays[spec.Index])
    return makeItems(spec), nil
}
items, _, _ := runBatchesParallel(ctx, batches, 5, gen, nil)
// assert items appear in batch order, not completion order

// 2. Partial-success → warnings, not error
gen := func(_, spec, _) ([]Item, error) {
    if spec.Index == 1 { return nil, errors.New("simulated batch 1 failure") }
    return makeItems(spec), nil
}
items, warnings, err := runBatchesParallel(ctx, batches, 5, gen, nil)
// err == nil, len(warnings) == 1, items contain only batches 0 and 2

// 3. Emit serialisation under -race
var inside int32
emit := func(_, _) {
    if got := atomic.AddInt32(&inside, 1); got != 1 {
        t.Errorf("concurrent emit: inside = %d", got)
    }
    time.Sleep(time.Microsecond)
    atomic.AddInt32(&inside, -1)
}
runBatchesParallel(ctx, batches, 8, gen, emit)

// 4. Max-parallel cap honoured (atomic counter for in-flight goroutines)
//    See content_plan/parallel_test.go for the canonical implementation.
```

For an integration test against the real model: deliberately force multiple batches by setting `MaxPostsPerBatch=5` with a mid-range `EstimatedPostCount=10`, then assert (a) the response shape is valid, (b) all dimensions are represented across batches, and (c) emitted `Index` values form a unique set with the lowest at 0. ~$0.005/run on Haiku makes this safe to leave on without budgeting CI cost.

### Gotchas specific to batching

1. **Single-shot fallback is a feature, not a leftover.** When `totalCount` is unset or `planBatches` returns nil, fall through to one un-batched call. Preserves the pre-batching behaviour for callers that don't pre-compute counts.
2. **Mutex the emit callback even when "obviously" safe.** SSE writers are single-writer; missing the lock surfaces as garbled events under load, not a clean panic. Race-detector clean is the bar.
3. **Partial-success aggregation is the default.** A 4-batch run with one batch failing should return 90 of 120 items + a warning, not abort everything. Hard `*AIError` fires only when **every** batch fails so the SSE error event surfaces a real outage rather than a flake.
4. **Aggregate in batch order, not completion order.** Persisting items as they arrive would scatter them across whatever order goroutines happened to finish in. The slot allocator's deterministic ordering is the contract; honour it on the way out.
5. **Don't share a Genkit instance across rebuilds.** If your project rotates Anthropic keys at runtime (e.g. via the SecretStore pattern from CON-64), the Anthropic plugin captures the key at `Init` and panics on re-init. Keep the embedding genkit instance separate from the rebuildable Anthropic-flow instance so an Anthropic key change doesn't disturb the embedder.
6. **Front-end consequence.** Items arriving out of slot order is now the contract — UIs must place by `Index`, not by stream position. Document this on the `*EventPayload.Index` field comment so the next person to wire a frontend doesn't append-as-arrived and ship a jumbled view.

---

## Variant: per-post persistence for streamed records

When a flow streams an array of records (posts, summaries, classifications, generated invoices, …) and the records have meaningful database identity, persist each one **inline as it parses**, not as a final aggregate insert. Canonical reference: `src/genkit/flows/content_plan/` after CON-66 — applied on top of the CON-67 batching path.

### When to reach for this

- The flow's output is an array of records that will be persisted regardless of run success.
- The user has already seen the streamed previews in the UI by the time the run finishes — discarding them on a late-stage failure is a worse UX than honouring what's already on screen.
- You want partial-success semantics: one batch failing in a parallel run shouldn't roll back the others.
- Symptoms motivating it: bug reports of "the calendar showed 90 posts but then everything disappeared," "client refresh during generation lost all my work," or "DB write failed on the final commit and we lost the entire run."

### What it changes vs. the canonical Step 7

```
Canonical:                            With per-post persistence:
─────                                 ──────────────────────────
parse stream → []Records              parse stream → for each record:
                                        ↳ validate inline
validateOutput(records) → valid set     ↳ persist inline (single-row insert)
                                        ↳ emit "post" event with id
persistDraftPosts(valid) → bulk        ↳ next record
   insert at the end                  (no separate persist step)
emit "complete" with valid set        emit "complete" with the persisted set
```

Steps 5 (validateOutput) and 6 (persistDraftPosts) become **no-op SSE step events** for client compatibility; the substantive work happened inside Step 4. The response struct's `Posts: ...` field now reflects the inline-persisted set rather than a post-validation slice.

### File-level changes

1. **`validate.go`** — extract the per-record check into a closure. Replaces the slice-based `validateOutput` as the source of truth (the legacy slice wrapper stays as a thin convenience for tests):
   ```go
   type postValidator func(post DraftPost) error

   func buildPostValidator(campaign *models.Campaign, platforms []resolvedPlatform) postValidator {
       // ... build platform/phase/date allowlists once ...
       return func(post DraftPost) error {
           // ... per-rule rejection with descriptive errors ...
       }
   }
   ```

2. **`generate.go`** — `persistOne(ctx, dp, campaign, postRepo) (string, error)` replaces the bulk `persistDraftPosts`. Returns the new row ID. `generatePostsStreaming` gains two parameters threaded down from `generatePosts`:
   ```go
   func generatePostsStreaming(
       ctx context.Context,
       g *genkit.Genkit,
       modelName, systemPrompt, userPrompt string,
       maxOutputTokens int64,
       globalStartIndex int,
       validate postValidator,
       persistFn func(ctx context.Context, post DraftPost) (string, error),
       onEvent OnEventFunc,
   ) ([]DraftPost, error) {
       // ... within the streaming chunk loop, for each parsed post:
       tryPersist := func(post DraftPost, position int) {
           if err := validate(post); err != nil {
               emit(onEvent, SSEEventWarning, WarningPayload{
                   Message: fmt.Sprintf("post %q dropped: %s", post.Title, err),
               })
               return
           }
           id, err := persistFn(ctx, post)
           if err != nil {
               emit(onEvent, SSEEventWarning, WarningPayload{
                   Message: fmt.Sprintf("post %q persist failed: %v", post.Title, err),
               })
               return
           }
           persistedPositions[position] = true
           emit(onEvent, SSEEventPost, PostEventPayload{
               Post:  post,
               Index: globalStartIndex + len(posts),
               ID:    id,
           })
           posts = append(posts, post)
       }
       // ...
   }
   ```

3. **`generate.go` (continued)** — `generatePosts` builds the validator + persist closure once per run and threads them into every batch:
   ```go
   validate := buildPostValidator(campaign, platforms)
   persistFn := func(ctx context.Context, dp DraftPost) (string, error) {
       return persistOne(ctx, dp, campaign, repos.Posts)
   }
   // ... pass into single-shot fallback AND each batch's gen closure ...
   ```

4. **`flow.go` / `runX`** — Step 4 absorbs validation + persistence; Steps 5 and 6 become no-op SSE step emits for client compatibility. The response's `Posts: ...` field uses the inline-persisted set:
   ```go
   posts, genWarnings, err := generatePosts(ctx, g, campaign, platforms, assets, cfg, repos, onEvent)
   if err != nil {
       // Even on error, surviving persisted posts are returned so the
       // caller can surface partial success.
       return nil, err
   }
   emit(onEvent, SSEEventStep, StepEventPayload{Step: "generatePosts", Status: "done"})

   // No-ops, kept for client compat:
   emit(onEvent, SSEEventStep, StepEventPayload{Step: "validateOutput", Status: "done"})
   emit(onEvent, SSEEventStep, StepEventPayload{Step: "persistDraftPosts", Status: "done"})

   return &ContentPlanResponse{Posts: posts, Warnings: warnings, /* ... */}, nil
   ```

5. **`types.go`** — three additive changes:
   ```go
   const SSEEventWarning SSEEventKind = "warning"

   type WarningPayload struct {
       Message string `json:"message"`
       Index   int    `json:"index,omitempty"`
   }

   type PostEventPayload struct {
       Post  DraftPost `json:"post"`
       Index int       `json:"index"`
       ID    string    `json:"id"`  // NEW: persisted row ID
   }
   ```

### Stream-fallback dedup

`generatePostsStreaming` retries with a blocking `genkit.Generate` when the streaming connection fails. Without dedup, every post that already streamed-and-persisted would be parsed again from the blocking response and inserted a second time. Track raw response positions in a `persistedPositions map[int]bool`:

```go
parsedPosition := 0  // 0-based count of every parse attempt (valid + invalid)

// streaming loop:
for _, raw := range scanner.push(chunkText) {
    position := parsedPosition
    parsedPosition++
    post, ok := parseAndTrimPost(raw)
    if !ok { continue }
    tryPersist(post, position)  // sets persistedPositions[position] on success
}

// fallback loop:
for i, post := range fallbackPosts {
    if persistedPositions[i] {
        continue  // first-write wins, even if the blocking response
                  // produces a slightly different text for this slot
                  // (non-zero temperature)
    }
    tryPersist(post, i)
}
```

Position-based dedup assumes the model is approximately deterministic in its first-N positions — true under temperature 0 and effectively true under low temperatures with a structured prompt. If your flow uses high-temperature creative generation, factor that in (consider content-hash-based dedup).

### Layering with parallel batching

Critical detail when combining per-post persistence with the parallel-batching variant: **`runBatchesParallel` must NOT discard a batch's partial posts on error.** The default-looking shape would be:

```go
posts, err := gen(ctx, spec, safeEmit)
if err != nil {
    results[i] = batchResult{err: err}  // ← BUG: persisted posts lost from response slice
    return
}
results[i] = batchResult{posts: posts}
```

The right shape — keep the partial slice even when err is non-nil:

```go
posts, err := gen(ctx, spec, safeEmit)
results[i] = batchResult{posts: posts, err: err}
// ...
// In aggregation:
allPosts = append(allPosts, r.posts...)  // always include
if r.err != nil { warnings = append(warnings, ...) }
```

The DB has the rows regardless; the response must reflect that. Same rule applies to the all-failed `*AIError` path: return `allPosts, warnings, &AIError{...}` rather than discarding.

### SSE contract changes

| Event | Before | After per-post persistence |
|---|---|---|
| `post` | `{post, index}` — preview only | `{post, index, id}` — already in DB |
| `warning` | not emitted (only inside `complete.warnings`) | emitted live for every dropped or failed post |
| `complete` | full `Posts[]` + aggregated `warnings[]` | unchanged shape; `Posts[]` is the persisted set |
| `error` | terminal, response usually empty | terminal, surviving persisted posts still in DB and in the response |

Both new fields are additive — old clients that don't read `payload.id` or that ignore `warning` events keep working. Document the new contract on `PostEventPayload.ID` and `WarningPayload.Message` so the next person to wire a frontend uses them.

### Tests

- **Unit tests for `buildPostValidator`** — accept path, reject paths (unknown platform, contentType not in allowlist, empty allowlist passthrough, out-of-range date, unknown phase). Pure function, easy.
- **Slice wrapper test** — `validateOutput` (the retained legacy form) still produces the right warnings. Confirms the refactor preserves behaviour.
- **Integration test addition** — after a successful run, assert (a) every emitted `PostEventPayload.ID` is non-empty, (b) the DB row count for the campaign equals the streamed event count, (c) every emitted ID references a real row. Same fixture as the parallel-batching integration test; one extra `It` block.

`generatePostsStreaming` itself is hard to unit-test without faking the genkit stream. Lean on the integration test for end-to-end coverage; the validator and persist helpers are individually testable.

### Gotchas specific to per-post persistence

1. **Cross-run idempotency is not free.** A re-trigger of the flow for the same campaign creates fresh rows — same as the pre-CON-66 bulk-insert behaviour. If you need re-trigger to be a true upsert, design a stable per-slot key (e.g. `campaign_id + slot_index`) and `INSERT ... ON CONFLICT DO UPDATE`. Out of scope for the variant; surface explicitly in the PR if it matters.
2. **Validation moves earlier; warnings now arrive live.** Old `complete.warnings` arrived in one batch at the end; live `warning` events stream across the run. Both shapes coexist (`complete.warnings` aggregates everything emitted live); the React layer should handle either independently.
3. **DB-write contention.** Postgres (the datastore since CON-87) runs writers concurrently — no SQLite single-writer serialisation — so per-post inserts of distinct rows proceed in parallel, generally faster than the old WAL + `MaxOpenConns(1)` path. CON-87 caveat: if an insert derives a value via `MAX(...)+1` (next position / version number) guarded by a UNIQUE constraint, two concurrent inserts to the **same parent** can both read the same MAX and collide. Serialise those by locking the parent row first (`SELECT 1 FROM <parent> WHERE id = ? FOR UPDATE` inside the tx — see `CreateAtNextPosition` / `restore.go`). Independent per-post rows (distinct Sqid ids) have no such hazard.
4. **First-write-wins on stream→fallback dedup.** The streamed copy of a post is preserved even if the blocking response would have produced a slightly different text for that slot. This is usually what you want (the user already saw the streamed version) but document it for posterity.
5. **The flow's response is the persisted set.** No "valid posts" vs "raw posts" distinction any more. Tests that previously asserted on `validateOutput`'s output should now assert against the response's `Posts` field directly.
6. **Don't forget `repos` in `generatePosts`.** The persist closure needs the post repository; thread `ContentPlanRepos` (or your equivalent) through `generatePosts` if it wasn't already a parameter.

---

## Gotchas (read this before you start)

1. **Native Anthropic plugin, not compat.** `import "github.com/firebase/genkit/go/plugins/anthropic"`. The `plugins/compat_oai/anthropic` shim rejects `anthropic.MessageNewParams` as an unknown config type — the error is `unexpected config type: anthropic.MessageNewParams` which is misleading because the type name in the error matches the supported type (different package with the same short name).

2. **Prompt example ordering controls streaming order.** Without `WithOutputType` (see #11) the model isn't constrained to a server-side schema — it tracks the JSON object example you put in the prompt. Put user-visible fields (explanation, content) before metadata (flags, enums) in the example so they start streaming first. Struct field order in `<FlowName>Response` only affects how Go marshals the response when persisting to history — it does NOT affect the model's emission order anymore.

3. **Fingerprint every prompt field.** The context cache keyed only on one field (e.g. `post.Content`) silently returns stale data when any other prompt-affecting field changes. Use the unit-separator fingerprint pattern. When adding a new prompt field, update the fingerprint helper in the same commit.

4. **Dedupe tool events by Ref.** Anthropic streams partial tool requests as the input JSON is built. Emit `tool_call` only when `Partial == false`. Use a local map to dedupe by `Ref` in case the same complete tool use arrives in a later chunk.

5. **`context.Background()` in SSE handlers.** The fiber request ctx is cancelled the moment the handler returns `nil`, which happens BEFORE `SetBodyStreamWriter` runs its callback. Passing `c.Context()` to the runner will make every streaming call fail with context-cancelled after ~0ms.

6. **`Aggregated` chunks double-feed.** Check `chunk.Aggregated == false` before feeding text into the scanner. Aggregated chunks are recap frames that would append the full response text a second time and corrupt scanner state.

7. **Skip `partial` tool requests for UI.** `part.ToolRequest.Partial == true` means the model is still building the tool's input JSON. If you emit these, the UI sees a flicker of half-built inputs.

8. **Single `genkit.Init` for all plugins.** Flow discovery and the Dev UI break if plugins are split across Init calls. See commit `993fb8b CON-42: Fix flow discovery by passing all plugins to single genkit.Init`.

9. **Never log the model response.** It may contain user data. Log tokens / finish reason / duration, not text.

10. **Real DB in tests.** No mocks. `mustOpenTestDBWithMigrations()`. Tests at the handler layer, stubbing only the flow callback.

11. **`ai.WithOutputType` and `json.Unmarshal` are both traps.** They look like the obvious tools and they hard-fail on common Claude JSON drift. Use the `JSONStringScanner` for parsing — every flow, streaming or not. See Step 6 for the full table of drift patterns the scanner handles vs. what `json.Unmarshal` chokes on.

12. **Size `MaxTurns` for the flow's tool depth.** With tools enabled, `MaxTurns=N` allows up to `N-1` tool calls plus one final answer — the model needs an extra turn after its last tool call to produce the response. Genkit's library default is 5; we found 3 catastrophically low (asset-incorporation flows blow it on the second tool call) and recommend **8** as the FlowConfig default. Symptoms of too low: `model call failed: exceeded maximum tool call iterations (N)`. Symptoms of too high: a buggy prompt with tools can loop indefinitely (rare in practice — Claude is good at terminating). Tune via `<FlowName>FlowConfig.MaxTurns`; leaving it at 0 picks the default in `run.go`.

13. **Don't fail the call when only metadata is missing.** Truncation at `max_tokens` drops the trailing fields (action, flags, etc.) before it drops the long content. Use the graceful-degradation pattern from Step 6: fail only when the response is genuinely unusable (no content **and** no explanation); infer `action = "edited"` from non-empty content; provide a placeholder explanation. The user has already seen the streamed content — surfacing an "expected fields missing" error here would discard it for no benefit. Default `MaxOutputTokens` to **32768** to make truncation rare in the first place.

14. **Authorization before lookup.** If the flow operates on a user-owned resource, check ownership BEFORE returning 404 — never reveal whether an id exists to an unauthorized caller. The handler should 403 for "exists but not yours" and 404 for "doesn't exist or not yours" (whichever the project convention dictates — check `CLAUDE.md` / memory).

15. **Tool-using flows pay a hidden ~50s per request unless tool order is stable (CON-112).** The Genkit Anthropic plugin builds the outgoing `tools` array by ranging a Go **map** (`firebase/genkit go/ai/generate.go`), so tools ship in a **random order on every request**, and it forces `strict: true` on every tool (`plugins/internal/anthropic`). Anthropic compiles strict tool schemas into a constrained-decoding grammar cached **per exact tool-set with an order-sensitive key**, so a random order misses that cache on every call and pays the full server-side grammar compile (~50s) — chronically, nondeterministically, on every tool-using turn. **This is already fixed globally:** `server.InstallAnthropicToolOrderStabilizer()` wraps `http.DefaultTransport` to sort the `tools` array by name before every `/v1/messages` (installed in `newGenkitRuntime` before the plugin builds its client; gated by `ANTHROPIC_STABLE_TOOL_ORDER`, default on). A stable order keeps the cache warm and, as a bonus, makes tool selection deterministic. **When you add a new tool-using flow,** also wire the cold-start pre-warm: a `PrewarmTools bool` on the FlowConfig (set from `cfg.AnthropicStableToolOrder` in the server adapter) and a background `go prewarmToolCache(g, cfg, tools)` right after `defineTools(g)` in `InitX` — mirror `campaign_assistant`/`post_assistant`'s `prewarm.go`. It fires one throwaway `max_tokens:1` generation carrying the full tool set so the *first* request never eats the ~50s compile (`WithMaxTurns(1)` + `max_tokens:1` guarantees no tool actually executes during warm-up). **Diagnosis, if you ever see it regress:** run with `ANTHROPIC_DEBUG_HTTP=1` — a single `/v1/messages` with `server_ms≈50000` and `conn_ms≈0` means the time is server-side generation (compile), *not* your code; then diff the outgoing request bytes against a fast run to spot the ordering. Never diagnose "it's slow" from wall-clock alone — dump the bytes. See `src/transport/server/anthropic_tool_order.go`.

---

## Step 13: Verify

```bash
/usr/local/go/bin/go build ./src/...
/usr/local/go/bin/go vet ./src/...
/usr/local/go/bin/go test -count=1 ./src/genkit/flows/<flow_name>/... ./src/transport/handlers/...
/usr/local/go/bin/go build -tags integration ./src/integration/...   # if you added an integration test
```

Fix any compile or test errors before reporting success. Do not claim success without a green run.

---

## Checklist (complete in order)

- Prompt template created (`src/genkit/flows/<flow_name>/prompts/<flow_name>.tmpl`) with `system` + `context` blocks; user-visible fields appear first in the example JSON
- `types.go` created — request, response, config, repos, errors, SSE events (if streaming)
- **`scanner.go` + `scanner_test.go` copied verbatim** from `src/genkit/flows/post_assistant/` — every flow uses the scanner, even non-streaming ones
- `tools.go` created (if tools needed) — tool names match regex, descriptions written for LLM audience
- **(Tool-using flows) cold-start pre-warm wired (CON-112, Gotcha #15)** — `PrewarmTools bool` on the FlowConfig set from `cfg.AnthropicStableToolOrder` in the server adapter; `prewarm.go` with `prewarmToolCache` (throwaway `max_tokens:1` gen over the full tool set); `go prewarmToolCache(g, cfg, tools)` fired after `defineTools(g)` in `InitX`. The global `InstallAnthropicToolOrderStabilizer` already keeps the strict-tool grammar cache warm.
- `context.go` created with `fingerprint` cache (if static context) — every prompt field in the fingerprint
- `run.go` created — streaming callback feeds the scanner, **no `ai.WithOutputType`**, response struct populated from `scanner.Values()` with required-field sanity check, tool dedup, error mapping
- `flow.go` created — `InitX`, `NewXCallback`, `emit` helper
- `src/transport/server/<flow_name>.go` adapter created
- `src/transport/server/server.go` edited — callback var + `initX` call + handler registration gated on API key
- **Usage metering wired (CON-86, Step 8b)** — `Provider`/`Recorder`/`Checker` in `FlowConfig`; `cfg.Checker.Enforce(ctx)` gate placed after any cache short-circuit; `u.Record(ctx, resp)` after each completed call; model resolved via `modelconfig.Resolve(ctx, flow, slot)` (slot registered in the catalog + `SeedDefaults`), called with `Provider.CallMiddleware(flow, u.Record)` + `Provider.CallConfig(mc.Model, maxTokens)`; `initX` threads `provider, recorder, checker`; handler maps `*usage.LimitExceededError` → 402
- HTTP handler added to `src/transport/handlers/<resource>.go` — SSE or JSON shape, swagger comments, service-unavailable branch
- Callback threaded through `New<Resource>Handler` constructor signature
- Handler test added (`src/transport/handlers/<resource>_test.go`) — stub callback, SSE frame parser, happy path + error path
- (Optional) Integration test added under `src/integration/` with `//go:build integration` tag
- (Optional) React prototype wiring — SSE reader, per-chunk render, tool chips, spinner
- (Optional, when batching) `batch.go` slot allocator + `batch_test.go` distribution tests; `runBatchesParallel` extracted with stubbed-gen unit tests covering ordered aggregation, partial-success warnings, all-failed → `*AIError`, max-parallel cap, and emit non-reentrancy under `-race`; user prompt template wraps slot table in `{{if .Batch}}` with single-shot fallback preserved; `MaxPostsPerBatch` and `MaxParallelBatches` plumbed through config + server adapter; per-event `Index` doc updated to reflect stable-slot semantics
- (Optional, when persisting incrementally) `buildPostValidator` extracted from `validateOutput` + unit tests; `persistOne` replaces bulk persist with single-row insert returning the new ID; `generatePostsStreaming` takes `validate` and `persistFn` params and inline-validates / inline-persists / inline-emits each parsed record; stream→fallback dedup via a `persistedPositions` map; `runBatchesParallel` keeps each batch's partial posts on error (don't discard `posts` when `err != nil`); `PostEventPayload.ID` and `SSEEventWarning` / `WarningPayload` added to `types.go` (additive); legacy `validateOutput` / `persistDraftPosts` SSE step events fire as no-ops for client compat; integration test asserts every emitted ID maps to a real DB row
- `go build ./src/...` clean
- `go test -count=1 ./src/...` green — including all `TestScanner_*`, `TestValues_*`, and `TestTrimIncompleteUTF8`
