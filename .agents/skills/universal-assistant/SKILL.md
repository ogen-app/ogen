---
name: universal-assistant
description: How the universal assistant works (one orchestrator, core tools plus tool packs loaded by screen and scope, offers, undo) and how to add a pack or a tool to it. Use when the user asks to give the assistant a new capability, add a pack or tool, change what a screen's assistant can do, or debug a turn, an offer, an undo or pack loading.
tools: Read, Edit, Write, Glob, Grep, Bash
---

# Universal Assistant Skill

One assistant serves every screen. A turn is one orchestrator call with the core tools plus the tool packs of the screen and scope the message came from. Capabilities are added as **packs** and **tools**, never as new assistants or endpoints.

Canonical code:

- `src/genkit/flows/assistant/`: the runtime (`run.go` turn, `pack.go` packs and `Turn.Subject`, `tools.go` core tools, `proposals.go` offers, `route.go` dry-run for evals)
- `src/genkit/flows/internal/assistantkit/`: the tool contract (`spec.go`, `outcome.go`, `registry.go`, `wrap.go`, `proposal.go` for `Gate`/`Gated`/`Offer`, `touched.go`)
- `src/genkit/flows/assistant/packs/`: domain packs. `workspace.go` is the smallest; `ideas.go` shows gated bulk tools and undo.
- `src/genkit/flows/campaign_assistant/`, `src/genkit/flows/post_assistant/`: the two packs with `Begin`/`End` (subject block, results, notifications)
- `src/usecase/assistantundo/`: undo
- `src/transport/server/deps.go` (`packDeps`), `genkit_runtime.go`: wiring

## How a turn runs

1. `POST /api/assistant` with `{instruction, viewContext}`. `domain/viewcontext` validates it and `usecase/viewresolve` resolves it. Errors return 400/404 before SSE opens.
2. Spend cap check, then history for the thread scope (`workspace`, `campaign:<id>`, `post:<id>`).
3. The default packs for the screen and scope load. Each pack's `Begin` resolves its subject with `t.Subject(kind)`: the offer's own `<kind>Id` when confirming one; else the thread's entity of that kind, or none when a different one of that kind is in focus (ambiguous); else the entity in focus. It returns a block that follows `<view>`. A `Refused` from `Begin` drops the pack and tells the model why.
4. One model call: cached system prompt (core rules + loaded packs' `Instructions` + generated tool guide), history, then `<view>` + pack blocks + message. Tool rounds are capped by `MaxTurns`.
5. `loadPack` restarts the call once with one more pack, replaying the rounds already done.
6. Tools run through the registry: gate, then outcome. A gated call that needs a yes becomes an **offer** instead of running.
7. The reply is the final round's text. Each pack's `End` reports `results.<pack>`, and wins when tool rounds ran out.
8. The turn is persisted. A failed write sets `unsaved`; it never fails the turn.
9. The stream ends with `proposal_created`/`proposal_resolved`, `ui_action`s, then `complete`. If the model call fails after work was committed, `End` still runs.

## The tool contract (assistantkit)

- **Tier** labels what a tool may do: `TierRead`, `TierReversible` (single undoable change) or `TierProposal` (bulk, generative, scheduling, external or destructive). Nothing enforces a tier at run time beyond checking it is valid. **Only the gate holds a call back**, so a proposal-tier tool must be wrapped in `assistantkit.Gated`.
- **Gate** (`assistantkit.Gated(name, Gate[In]{Precheck, Needs, Summary}, tool)`):
  - `Precheck` refuses bad input before any offer.
  - `Needs` decides when the call becomes an offer. **`Needs` nil means every call asks first.** Bulk work that may run small requests directly uses `Needs: overN(d, n)`, where N is `ASSISTANT_PROPOSAL_THRESHOLD` (default 3).
  - `Summary` (or `Describe`) is the card text.
  - A confirmed call (`assistantkit.Confirmed(ctx)`) skips the gate and runs.
- **Outcome**: every tool returns `{status, result}`, with status `ok`, `refused` (`reason`, `alternatives`), `conflict` (`options`) or `proposed`.
  - Return user-facing problems as `assistantkit.Refused("…")`, never as a Go error. A Go error aborts the turn.
- **Touch**: `assistantkit.Touch(ctx, "<kind>", id)` marks what changed, so the client refetches through `complete.touched`.
- **Strict schema**: core tools are Anthropic-strict. **Every pack tool must pass `ai.WithStrictSchema(false)`.**
  - Anthropic rejects a request with more than 20 strict tools.
  - `TestEveryPackRegistersTogether` fails if you forget.
- **Spec options**:
  - `Prepare` claims a resource, such as the campaign's heavy slot, before an offer is answered.
  - `Free` lets a card confirmation through past the spend cap when the work makes no model call.
- **Names**: tool names are unique across the whole genkit registry, which every flow shares. A duplicate panics at boot.

## Adding a pack

### Step 0: Decide

1. **What the pack does**: one sentence. It becomes `Summary`, which `loadPack` shows the model.
2. **Where it loads by default**: `Screens` and/or `Scopes`. Every screen must be in `viewcontext.screens` (`src/domain/viewcontext/viewcontext.go`); add new ones there. Keep default sets small, because each distinct set is a cached prompt prefix that is prewarmed at boot.
3. **Does it need a subject?** If it works on one campaign or post, add a `Begin` that reads `t.Subject(kind)`. Otherwise read `assistant.CampaignID(ctx)` / `assistant.Scope(ctx)` inside tools.
4. **The tier of each tool**: read, reversible (needs an undoer) or proposal (bulk, destructive or plan-wide; needs a gate).

### Step 1: Put the logic in a service

The pack is a thin adapter. Domain rules live in `src/usecase/<area>`, shared with the REST handler, so a tool and its endpoint never disagree. If a REST endpoint does the same thing, call its service.

### Step 2: Define the pack

`src/genkit/flows/assistant/packs/<name>.go`:

```go
const reportsPackName = "reports"

// reportsPack ... (one line on what it answers).
func reportsPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    reportsPackName,
		Summary: "what the user can ask for, in plain words",
		Screens: []viewcontext.Screen{"reports"},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			get := genkit.DefineTool(g, "getReport",
				"What it returns, for the model.",
				assistantkit.Wrap(func(ctx context.Context, in GetReportInput) (*ReportOut, error) { return getReport(ctx, d, in) }),
				ai.WithStrictSchema(false))
			return []assistantkit.Spec{
				spec(reportsPackName, get, assistantkit.TierRead,
					`Call this when the user asks … — "example phrasing", "another". Defaults: …`),
			}, nil
		},
		// Instructions: "rules that apply only while this pack is loaded",
	}
}
```

- **Inputs**: `json` tags in camelCase, plus `jsonschema:"description=…"` stating defaults. Optional fields use `omitempty`.
- **Guide strings** (`spec`'s last argument): say when to call the tool with quoted user phrasings, the defaults, and what to do on refusal. The registry renders them into the prompt.
- **Refusals**: map service sentinel errors to `assistantkit.Refused(...)` (see `activityReport` in `workspace.go`).
- **Bulk tools**: report partial progress, with failures listed and stopped early with a reason, rather than failing the whole call (see `stoppedEarly` in `ideas.go`).

### Step 3: Gate writes that need a yes

```go
genkit.DefineTool(g, "archiveReports", "… Above a few it is offered for the user to confirm first.",
	assistantkit.Gated("archiveReports", assistantkit.Gate[ArchiveInput]{
		Precheck: func(_ context.Context, in ArchiveInput) error { return checkIDs(in.IDs) },
		Needs:    func(_ context.Context, in ArchiveInput) bool { return overN(d, len(in.IDs)) },
		Summary:  func(in ArchiveInput) string { return fmt.Sprintf("Archive %s", plural(len(in.IDs), "report", "reports")) },
	}, assistantkit.Wrap(func(ctx context.Context, in ArchiveInput) (*BulkOut, error) { return archiveReports(ctx, d, in) })),
	ai.WithStrictSchema(false))
```

Spec it as `TierProposal`. The gate, not the tier, decides when it asks:

- **Always ask** (delete, move, regenerate a plan): leave `Needs` out. Every unconfirmed call becomes an offer.
- **Ask above a threshold** (bulk verdicts): `Needs: overN(d, n)`. Calls at or below N run directly, so make sure running them without a yes is acceptable.

### Step 4: Make reversible writes undoable

1. Add an `AssistantActionKind` in `src/domain/models/assistant_action.go`.
2. In the tool, after the write, call `recordUndo(ctx, d, kind, entityID, before, after)` with JSON snapshots of only the fields you changed. Return the undo id in the output.
   - `recordUndo` is best effort. If logging fails it returns `""`, and the write stands without an undo, so the output's undo id is empty. The model must not then promise undo; say so in the tool's guide.
   - When undo must be guaranteed, make the write and the log one transaction. Write through repository methods that take `bun.IDB`, and record the action with a transaction-aware create in the same `RunInTx`. `AssistantActionRepository.Create` doesn't take a transaction today, so add a variant.
3. Add an `Undoer` in `src/usecase/assistantundo`. It runs inside the undo transaction (`tx bun.IDB`):
   - lock the entity;
   - return `ErrChangedSince` unless it still matches `After`;
   - restore `Before`.
4. Attach the undoer with a `With<Area>(repo)` method, and chain it in `routes.go` where `assistantundo.New(...)` is built.
5. Repository writes used by undo take `bun.IDB` so they join the transaction. Hydrate on that same `db`.

### Step 5: Register and wire

1. Add the pack to `packs.All` in `packs.go`, guarded by its dependencies being non-nil. Its position sets the tool order.
2. Add any new dependencies to `packs.Deps`, and set them in `packDeps` in `src/transport/server/deps.go`.
3. If the pack needs a subject block and `End` results, model it on `campaign_assistant/pack.go` (`NewPack`, `Begin`, `End`, state on ctx).

### Step 6: Tests

- **Unit**: call the tool functions with fake repositories (see `packs_test.go`: `turn(t, scope, onEvent)` gives a tool context and a `touched()` reader). Cover:
  - refusals;
  - partial failure;
  - the gate's threshold;
  - `Touch`.
- **Undo**: a Postgres test that runs the tool, undoes it through `assistantundo.New(db, ...)`, and checks both restore and `ErrChangedSince` (see `assistantundo_test.go`, `foundation_test.go`).
- **Registration**: update the pack count in `TestEveryPackRegistersTogether` (`register_test.go`). It also checks that every pack tool is non-strict.
- **Evals**: add 1–3 cases to `evalCases` in `packs/evals_test.go`. Each case has the screen, the scope, a realistic message, the tools an acceptable first call may be (`want`) and those it must never be (`not: writes` for questions).
  - Run with `make evals`; it needs `ANTHROPIC_API_KEY`.
  - Keep the fixtures in the eval harness realistic, e.g. a post with draft theses for "write from theses".
- **Live**: if the pack drives a real end-to-end flow, add a spec to `src/integration` that runs through `newAssistant` and `runIn` (`assistant_turns_test.go`).
  - Assert outcomes that are stable under model variance. For example, accept either "offered, then confirmed" or "ran directly" when both are valid.

### Step 7: Verify

```sh
golangci-lint run --build-tags integration,evals ./src/...
TEST_DATABASE_DSN="postgres://ogen:ogen@localhost:5433/postgres?sslmode=disable" go test -timeout 40m ./src/...
make evals                # needs ANTHROPIC_API_KEY
make test-integration     # live suites; needs ANTHROPIC_API_KEY
```

## Adding a tool to an existing pack

Use Steps 2–4 and 6 inside that pack's `Define`. Watch three things:

- **New default sets**: a new default pack set adds a cached prefix.
- **Tool count**: tools per default set don't count toward the strict limit, because pack tools are non-strict, but each one adds prompt length.
- **Pack instructions**: if the tool changes how the model should behave across the pack, update the pack's `Instructions` (campaign: `prompts/campaign_assistant.tmpl`), not only the tool description.

## Pitfalls

- **A Go error from a tool aborts the whole turn.** Anything the user should hear is a `Refused`.
- **Don't call a write tool to answer a question.** Pack instructions should say questions are read-only, and evals cases with `not: writes` guard it.
- **Dates**: resolve from the CURRENT message against today in `<view>`, never from history.
- **Offers**: an offer made in this turn can't be confirmed in this turn, and identical calls in one turn share one offer.
- **Bound offers**: an offer bound to a subject must carry it in its args (`<kind>Id`), so confirming it from another screen begins the right pack (see `bindScheduledPost`).
- **Model selection**: it comes from Harbor model config (`assistant/orchestrator`, `assistant/writer`), never from env.
- **Middleware**: genkit v1.9 deprecates `ai.WithMiddleware`, and lint excludes it until the usage, caching and effort hooks move to `WithUse`. Don't add new middleware-based features.
