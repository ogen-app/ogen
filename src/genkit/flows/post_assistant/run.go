package post_assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// isPostRemovedFKViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503) on a post_id constraint — i.e. an insert whose
// post_id no longer matches a posts row. Every post-referencing write in this
// flow (post_assistant_messages, post_versions) points at posts(id) through a
// *_post_id_fkey constraint, so a concurrent post deletion trips exactly these.
func isPostRemovedFKViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok &&
		pgErr.Code == "23503" &&
		strings.HasSuffix(pgErr.ConstraintName, "_post_id_fkey")
}

func runPostAssistant(
	ctx context.Context,
	g *genkit.Genkit,
	req PostAssistantRequest,
	cfg PostAssistantFlowConfig,
	repos PostAssistantRepos,
	systemTmpl, contextTmpl *template.Template,
	writerInstructions string,
	tools *toolSet,
	onEvent OnEventFunc,
) (out *PostAssistantResponse, retErr error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "instruction_len", len(req.Instruction))

	// Enforcement gate (CON-86 FR9): block before any provider call when the
	// tenant is already over a cap in enforce mode. Nil checker = no gate.
	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}

	// finaliseOwnerID / finaliseTenantID are captured once the post is loaded so
	// the deferred finalisation event + durable notification can be scoped to the
	// post owner and tenant. Empty before load → finalisation for very-early
	// failures is skipped.
	var finaliseOwnerID, finaliseTenantID string

	defer func() {
		if finaliseOwnerID == "" {
			return
		}
		publishAssistantFinalised(cfg.Hub, req.PostID, finaliseOwnerID, out, retErr)
		// CON-285: a durable assistant finished/failed row for the initiator.
		notifyAssistantFinalised(cfg.Notifier, finaliseTenantID, finaliseOwnerID, req.PostID, out, retErr)
	}()

	if req.Instruction == "" {
		return nil, &ValidationError{Msg: "instruction is required"}
	}

	// ── Load post ────────────────────────────────────────────────────────────
	post, err := repos.Posts.GetByID(ctx, req.PostID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "post not found"}
		}
		return nil, fmt.Errorf("load post: %w", err)
	}
	finaliseOwnerID = post.CreatedBy
	finaliseTenantID = post.TenantID

	// ── Ensure initial version ───────────────────────────────────────────────
	count, err := repos.Versions.CountByPostID(ctx, req.PostID)
	if err != nil {
		return nil, fmt.Errorf("count versions: %w", err)
	}
	if count == 0 && post.Content != "" {
		id, err := models.NewID()
		if err != nil {
			return nil, err
		}
		if err := repos.Versions.Create(ctx, &models.PostVersion{
			ID:            id,
			PostID:        req.PostID,
			VersionNumber: 1,
			Content:       post.Content,
			Note:          "Initial version",
			Creator:       "user",
		}); err != nil {
			if isPostRemovedFKViolation(err) {
				slog.WarnContext(ctx, "post deleted mid-turn; discarding assistant result", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
				return nil, ErrPostRemovedDuringTurn
			}
			return nil, fmt.Errorf("create initial version: %w", err)
		}
		slog.InfoContext(ctx, "created initial version snapshot", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
	}

	// ── Assemble context + load history in parallel ─────────────────────────
	var actx *assistantContext
	var ctxErr error
	var history []*ai.Message
	var histErr error

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		actx, ctxErr = assembleContextCached(ctx, post, repos, systemTmpl, contextTmpl)
	}()
	go func() {
		defer wg.Done()
		msgs, err := repos.Messages.ListRecentByPostID(ctx, req.PostID, 10)
		if err != nil {
			histErr = err
			return
		}
		history = make([]*ai.Message, 0, len(msgs))
		for _, m := range msgs {
			switch m.Role {
			case "user":
				history = append(history, ai.NewUserTextMessage(m.Content))
			case "model":
				history = append(history, ai.NewModelTextMessage(m.Content))
			}
		}
	}()
	wg.Wait()

	if ctxErr != nil {
		return nil, fmt.Errorf("assemble context: %w", ctxErr)
	}
	if histErr != nil {
		return nil, fmt.Errorf("load history: %w", histErr)
	}

	// ── Inject per-request state for tools ───────────────────────────────────
	// Platforms power the clonePost tool's "Threads" → ID resolution.
	// Best-effort: a load failure just disables cross-platform clones.
	var platforms []models.Platform
	if repos.Platforms != nil {
		if ps, perr := repos.Platforms.List(ctx); perr == nil {
			platforms = ps
		} else {
			slog.WarnContext(ctx, "load platforms failed, clone name-resolution degraded", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, logging.AttrError, perr)
		}
	}
	st := &requestState{
		postID:      req.PostID,
		postStatus:  post.Status,
		assetIDs:    post.UsedAssetIDs,
		repos:       repos,
		embedder:    cfg.Embedder,
		cloneSvc:    cfg.CloneService,
		restoreSvc:  cfg.RestoreService,
		scheduleSvc: cfg.ScheduleService,
		noteSvc:     cfg.NoteService,
		actor:       post.CreatedBy,
		platforms:   platforms,
		onEvent:     onEvent,
		// Writer support (CON-128): the editPost tool + clonePost adaptation
		// run a nested Sonnet generation off this state.
		g:               g,
		provider:        cfg.Provider,
		recorder:        cfg.Recorder,
		writerMaxTokens: cfg.MaxOutputTokens,
	}
	// The writer's system prompt is the copywriter instructions plus the same
	// campaign/post context block the planner sees; only assembled in the
	// hybrid path (writerSystem stays empty in the legacy path, which disables
	// the writer helpers). Injecting the context here keeps the writer aware of
	// the campaign voice + current content without a second DB read.
	if cfg.PlannerEnabled {
		st.writerSystem = writerInstructions + "\n\n" + actx.ContextBlock
	}
	ctx = withRequestState(ctx, st)

	// CON-78: the scheduling context (current time + workspace timezone,
	// the post's status, its auto/manual routing, and a readiness summary)
	// changes every turn — current time most of all — so it is injected
	// fresh into the user turn rather than the cached system/context block,
	// preserving Anthropic prompt caching of the stable prefix.
	prompt := req.Instruction
	if cfg.ScheduleService != nil {
		if sb := buildSchedulingContext(ctx, post, repos); sb != "" {
			prompt = sb + "\n\n---\n\nUser instruction: " + req.Instruction
		}
	}

	// ── Call model ───────────────────────────────────────────────────────────
	// CON-128: in the hybrid path the orchestration loop routes on the cheap
	// planning model and delegates all copywriting to the Sonnet editPost
	// write-tool; the loop itself only emits a short envelope, so it takes a
	// small output cap. The legacy path keeps the single generation-model call
	// that writes the full post inline, so it needs the generous output budget.
	planner := cfg.PlannerEnabled
	// In the hybrid path the loop routes on the planner slot and delegates
	// copywriting to the writer slot (the editPost tool); in the legacy path the
	// single loop call does the writing itself, so it maps to the writer slot.
	loopSlot := modelconfig.SlotWriter
	loopMaxTokens := cfg.MaxOutputTokens
	if loopMaxTokens == 0 {
		loopMaxTokens = 64000
	}
	if planner {
		loopSlot = modelconfig.SlotPlanner
		loopMaxTokens = cfg.PlannerMaxOutputTokens
		if loopMaxTokens == 0 {
			loopMaxTokens = 8192
		}
	}
	maxTurns := cfg.MaxTurns
	if maxTurns == 0 {
		maxTurns = 8
	}

	modelName := modelconfig.Ref(ctx, modelconfig.FlowPostAssistant, loopSlot)

	// System + context block forms the stable cached prefix.
	systemBlock := actx.SystemPrompt + "\n\n" + actx.ContextBlock

	// Set up an incremental JSON scanner that watches the string-valued fields
	// whose deltas we surface to the client. The scanner decodes JSON escapes
	// as they arrive, so the client never sees raw \n / \uXXXX. In the hybrid
	// path the planner never emits updatedContent — the editPost writer sub-call
	// streams content_delta itself — so only explanation is watched. (Values()
	// still returns every top-level field for response assembly regardless.)
	watchFields := []string{"explanation", "updatedContent"}
	if planner {
		watchFields = []string{"explanation"}
	}
	scanner := jsonstream.New(
		watchFields,
		func(key, delta string) {
			switch key {
			case "explanation":
				emit(onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
			case "updatedContent":
				emit(onEvent, SSEEventContentDelta, DeltaEventPayload{Delta: delta})
			}
		},
	)

	// Tool calls arrive in many partial streaming fragments as the model
	// builds the input JSON; we only surface the final complete request.
	// Tool responses are small and emitted once. Both are deduped by Ref
	// since genkit may replay the same part in later chunks.
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
				if tr == nil || tr.Partial {
					continue
				}
				if emittedToolCalls[tr.Ref] {
					continue
				}
				emittedToolCalls[tr.Ref] = true
				emit(onEvent, SSEEventToolCall, ToolCallEventPayload{
					Name:  tr.Name,
					Input: tr.Input,
					Ref:   tr.Ref,
				})
			case part.IsToolResponse():
				tr := part.ToolResponse
				if tr == nil || emittedToolResults[tr.Ref] {
					continue
				}
				emittedToolResults[tr.Ref] = true
				emit(onEvent, SSEEventToolResult, ToolResultEventPayload{
					Name: tr.Name,
					Ref:  tr.Ref,
					OK:   true,
				})
			}
		}
		return nil
	}

	// Use streaming mode — the Anthropic API requires it for requests
	// that may involve tool calls (which can exceed the 10-minute timeout
	// for non-streaming requests). The streaming callback fans chunks out
	// as SSE events for the UI.
	// NB: we deliberately do NOT pass ai.WithOutputType — genkit's
	// post-generation schema validator parses the raw text strictly and
	// returns (nil, err) on any blemish (trailing comma, stray char, etc.),
	// discarding the full response. Dropping the constraint lets us do the
	// parse ourselves with a tolerant preprocessor below. Format discipline
	// is enforced via the prompt, which is already explicit.
	// Tool set: the editPost write-tool is attached only in the hybrid path;
	// the legacy loop writes content inline and never routes through it.
	toolRefs := []ai.ToolRef{tools.listAssets, tools.getAssetChunks, tools.searchAssetChunks, tools.getCurrentContent, tools.clonePost, tools.restoreVersion, tools.schedulePost, tools.createNote}
	if planner {
		toolRefs = append(toolRefs, tools.editPost)
	}

	resp, err := genkit.Generate(ctx, g,
		ai.WithModelName(modelName),
		ai.WithSystem(systemBlock),
		ai.WithMessages(history...),
		ai.WithPrompt(prompt),
		ai.WithTools(toolRefs...),
		ai.WithMaxTurns(maxTurns),
		ai.WithStreaming(streamCb),
		cfg.Provider.CallConfig(loopMaxTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}

	// Deterministic truncation signal: when Anthropic's stop_reason is
	// "max_tokens", genkit surfaces it as FinishReasonLength. Log loudly
	// so the cap can be tuned (env MAX_OUTPUT_TOKENS) before users see
	// the recovery branches kick in.
	if resp.FinishReason == ai.FinishReasonLength {
		var outputTokens int64
		if resp.Usage != nil {
			outputTokens = int64(resp.Usage.OutputTokens)
		}
		slog.WarnContext(ctx, "response truncated at max tokens", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "output_tokens", outputTokens, "cap", loopMaxTokens)
	}

	if resp.Usage != nil {
		slog.InfoContext(ctx, "tokens", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "input", resp.Usage.InputTokens, "output", resp.Usage.OutputTokens, "total", resp.Usage.InputTokens+resp.Usage.OutputTokens)
	}
	cfg.Recorder.RecordResp(ctx, modelconfig.Vendor(ctx, modelconfig.FlowPostAssistant, loopSlot), modelconfig.Model(ctx, modelconfig.FlowPostAssistant, loopSlot), "post_assistant", resp)

	// ── Assemble response from scanner ───────────────────────────────────────
	// The scanner has been processing every chunk in the streaming callback
	// above. Its Values() method returns the parsed top-level fields —
	// strings decoded, literals coerced — without going through
	// encoding/json. This bypasses the whole class of Claude JSON-drift
	// bugs (trailing commas, missing separators, preamble prose, literal
	// newlines inside strings, truncation) that would otherwise hard-fail
	// the final Unmarshal. See scanner_test.go TestValues_* for coverage.
	vals := scanner.Values()
	result := PostAssistantResponse{}
	if s, ok := vals["explanation"].(string); ok {
		result.Explanation = s
	}
	if s, ok := vals["updatedContent"].(string); ok {
		result.UpdatedContent = s
	}
	if s, ok := vals["action"].(string); ok {
		result.Action = s
	}
	if b, ok := vals["saveVersion"].(bool); ok {
		result.SaveVersion = b
	}
	if s, ok := vals["versionNote"].(string); ok {
		result.VersionNote = s
	}

	// ── Clone handling (CON-59) ──────────────────────────────────────────────
	// If the clonePost tool ran this turn, it is the authoritative outcome:
	// the source post is untouched, action is "cloned", and we attach the
	// new draft's id. Done before the "no usable fields" guard below so a
	// terse model reply can't mask a successful clone.
	if st.cloneResult != nil {
		result.Action = "cloned"
		result.UpdatedContent = ""
		result.SaveVersion = false
		if result.Explanation == "" {
			result.Explanation = fmt.Sprintf("Cloned this post into a new draft (#%s).", st.cloneResult.Post.ID)
		}
		result.CloneResult = &CloneResultPayload{
			NewPostID:  st.cloneResult.Post.ID,
			PlatformID: st.cloneResult.Post.PlatformID,
			PostType:   st.cloneResult.ResolvedPostType,
			Adapted:    st.cloneResult.Adapted,
		}
	}

	// ── Restore handling (CON-68) ────────────────────────────────────────────
	// If the restoreVersion tool ran this turn, it is the authoritative
	// outcome: the service has already swapped the post content and
	// appended the version(s), so action is "restored" and we surface the
	// restored content (for the editor) without re-running the edit path.
	if st.restoreResult != nil {
		rr := st.restoreResult
		result.Action = "restored"
		result.UpdatedContent = rr.Post.Content
		result.SaveVersion = false
		if result.Explanation == "" {
			if rr.NoOp {
				result.Explanation = fmt.Sprintf("The post already matches version %d — nothing to restore.", rr.RestoredFromVersion)
			} else {
				result.Explanation = fmt.Sprintf("Restored the post to version %d (saved as version %d). Your previous content is kept in the history.", rr.RestoredFromVersion, rr.NewVersionNumber)
			}
		}
		result.RestoreResult = &RestoreResultPayload{
			RestoredFromVersion: rr.RestoredFromVersion,
			NewVersionNumber:    rr.NewVersionNumber,
			NoOp:                rr.NoOp,
		}
	}

	// ── Schedule handling (CON-78) ───────────────────────────────────────────
	// If the schedulePost tool committed this turn, it is the authoritative
	// outcome: the post's status + scheduled_at are persisted by the shared
	// service, action is "scheduled", and content is untouched.
	if st.scheduleResult != nil {
		sr := st.scheduleResult
		result.Action = "scheduled"
		result.UpdatedContent = ""
		result.SaveVersion = false
		if result.Explanation == "" {
			mode := "auto-publish"
			if !sr.AutoPublish {
				mode = "manual publishing"
			}
			result.Explanation = fmt.Sprintf("Scheduled this post for %s (%s).",
				sr.ScheduledAt.Format("Jan 2, 2006 15:04 MST"), mode)
		}
		result.ScheduleResult = &ScheduleResultPayload{
			ScheduledAt: sr.ScheduledAt.Format(time.RFC3339),
			Status:      string(sr.Status),
			AutoPublish: sr.AutoPublish,
			Promoted:    sr.Promoted,
		}
	}

	// ── Edit handling (CON-128) ──────────────────────────────────────────────
	// In the hybrid path the editPost tool ran the Sonnet writer this turn; its
	// content is authoritative and the planner never emits it. The planner
	// supplies the metadata (action / saveVersion / versionNote) in its JSON
	// envelope, already parsed into result above. Runs before the note handling
	// so an edit-and-note turn is finalised as "edited" with the note attached.
	if st.editResult != nil {
		result.Action = "edited"
		result.UpdatedContent = st.editResult.Content
	}

	// ── Note handling (CON-188) ──────────────────────────────────────────────
	// The createNote tool persisted its notes at call time (origin=assistant).
	// Notes are additive: a turn may create notes on their own or alongside an
	// edit, so this never clears an edit's updatedContent. When notes are the
	// only effect — no content edit and no other authoritative tool action —
	// the action is "noted".
	if len(st.noteResults) > 0 {
		result.NotesCreated = make([]NotePayload, 0, len(st.noteResults))
		for _, n := range st.noteResults {
			result.NotesCreated = append(result.NotesCreated, NotePayload{
				ID:    n.ID,
				Type:  string(n.Type),
				Title: n.Title,
				Body:  n.Body,
			})
		}
		// Only claim a notes-only turn when there is no edited content. Guarding
		// on updatedContent == "" protects a combined edit-and-note turn that was
		// truncated before the model emitted action: the trailing switch below
		// then infers "edited" from the non-empty content, and the notes still
		// attach — we never discard the edit by clearing it here.
		if result.Action != "edited" && result.UpdatedContent == "" && st.cloneResult == nil && st.restoreResult == nil && st.scheduleResult == nil {
			result.Action = "noted"
			result.SaveVersion = false
		}
		// Ensure a usable explanation so the "no usable fields" guard below
		// doesn't misfire on a notes-only turn where the model left it empty.
		if result.Explanation == "" {
			if len(st.noteResults) == 1 {
				result.Explanation = "Saved a note."
			} else {
				result.Explanation = fmt.Sprintf("Saved %d notes.", len(st.noteResults))
			}
		}
	}

	// Hybrid safety (CON-128): only the editPost writer may produce post copy,
	// so an "edited" turn is legitimate ONLY when the writer actually ran
	// (st.editResult set). Without it the planner either applied no edit, or
	// emitted its own inline content in violation of the split — either way we
	// must not persist that content (an empty body would wipe the post; a
	// planner-written body would leak Haiku prose past the writer). Discard any
	// such content and downgrade: to a notes-only turn if notes were captured
	// this turn, otherwise to an answer, dropping the version snapshot. The
	// legacy path can't hit this — there content comes from the same call.
	if planner && result.Action == "edited" && st.editResult == nil {
		slog.WarnContext(ctx, "planner claimed an edit without invoking editPost; discarding any inline content", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
		result.UpdatedContent = ""
		if len(st.noteResults) > 0 {
			result.Action = "noted"
		} else {
			result.Action = "declined"
		}
		result.SaveVersion = false
		if result.Explanation == "" {
			result.Explanation = "I couldn't apply that edit — could you rephrase what you'd like changed?"
		}
	}

	// Pure-prose recovery: occasionally the model ignores the JSON
	// envelope entirely and answers in plain prose (often when the user
	// asks an informational question). Salvage the raw text as the
	// explanation of a "declined" response so the user at least sees
	// the answer in the chat bubble. The prompt is the proper fix —
	// this is the safety net for when the prompt fails to constrain.
	if result.Explanation == "" && result.UpdatedContent == "" {
		raw := strings.TrimSpace(scanner.FullText())
		if raw != "" && !strings.Contains(raw, "{") {
			slog.WarnContext(ctx, "model emitted prose-only response, treating as informational/declined", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "len", len(raw))
			result.Explanation = raw
			result.Action = "declined"
		}
	}

	// Graceful degradation against truncated responses (max_tokens hit
	// mid-content). Field order in the prompt is
	// explanation → updatedContent → action → saveVersion → versionNote,
	// so when the model runs out of tokens during updatedContent the
	// trailing metadata fields are the first to drop off. If we got
	// usable content, recover the missing fields from defaults instead
	// of failing the whole turn.
	switch {
	case result.Explanation == "" && result.UpdatedContent == "":
		// Genuinely unusable — neither field came through.
		raw := scanner.FullText()
		slog.ErrorContext(ctx, "scanner found no usable fields", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "len", len(raw), "raw_preview", logging.Preview(raw, 500))
		return nil, &AIError{Msg: "model response did not contain the expected fields"}

	case result.Action == "" && result.UpdatedContent != "":
		// The model wouldn't have emitted updatedContent if it had
		// decided to decline; infer "edited".
		slog.WarnContext(ctx, "action missing, inferring edited from non-empty updatedContent (likely max_tokens truncation)", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
		result.Action = "edited"
	}

	// Surface a generic explanation if the model got truncated before
	// it could write one.
	if result.Explanation == "" && result.UpdatedContent != "" {
		result.Explanation = "Updated post content."
	}

	// CON-251 backstop: a submitted post's content is locked. The planner
	// path already refuses in the editPost tool, but the legacy single-model
	// path writes content straight into updatedContent with no tool, so guard
	// the persist here too. Coerce the edit to a declined turn — drop the
	// content and version so nothing is written — and explain why, keeping the
	// response coherent rather than silently discarding the write.
	if result.Action == "edited" && post.Status.IsSubmitted() {
		slog.WarnContext(ctx, "refused content edit on submitted post", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "status", string(post.Status))
		result.Action = "declined"
		result.UpdatedContent = ""
		result.SaveVersion = false
		result.Explanation = "This post is " + string(post.Status) + ", so its content is locked and I can't change it. Unschedule it (or duplicate it into a new draft) if you'd like to make edits."
	}

	// Content is persisted and returned as Markdown. The frontend is the
	// only layer that converts to/from BlockNote JSON for editor rendering.

	// ── Persist conversation turn ────────────────────────────────────────────
	userMsgID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	if err := repos.Messages.Create(ctx, &models.PostAssistantMessage{
		ID:      userMsgID,
		PostID:  req.PostID,
		Role:    "user",
		Content: req.Instruction,
	}); err != nil {
		if isPostRemovedFKViolation(err) {
			slog.WarnContext(ctx, "post deleted mid-turn; discarding assistant result", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
			return nil, ErrPostRemovedDuringTurn
		}
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	// Persist the model turn as the same JSON shape the assistant emits —
	// minus `updatedContent`, which is bulky and would bloat history on
	// subsequent turns. Storing JSON lets the UI reload the action /
	// saveVersion / versionNote badges on page refresh without a round-trip
	// through a custom parse, and gives the model its own prior response
	// back in its native output format.
	modelMsgID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	historyJSON, err := json.Marshal(struct {
		Action      string `json:"action"`
		Explanation string `json:"explanation"`
		SaveVersion bool   `json:"saveVersion"`
		VersionNote string `json:"versionNote,omitempty"`
		NoteCount   int    `json:"noteCount,omitzero"`
	}{
		Action:      result.Action,
		Explanation: result.Explanation,
		SaveVersion: result.SaveVersion,
		VersionNote: result.VersionNote,
		NoteCount:   len(result.NotesCreated),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal model history: %w", err)
	}
	if err := repos.Messages.Create(ctx, &models.PostAssistantMessage{
		ID:      modelMsgID,
		PostID:  req.PostID,
		Role:    "model",
		Content: string(historyJSON),
	}); err != nil {
		if isPostRemovedFKViolation(err) {
			slog.WarnContext(ctx, "post deleted mid-turn; discarding assistant result", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
			return nil, ErrPostRemovedDuringTurn
		}
		return nil, fmt.Errorf("persist model message: %w", err)
	}

	// ── Handle versioning ────────────────────────────────────────────────────
	if result.SaveVersion && result.Action == "edited" {
		latest, err := repos.Versions.GetLatestByPostID(ctx, req.PostID)
		if err != nil {
			return nil, fmt.Errorf("get latest version: %w", err)
		}
		nextNum := 1
		if latest != nil {
			nextNum = latest.VersionNumber + 1
		}

		versionID, err := models.NewID()
		if err != nil {
			return nil, err
		}
		if err := repos.Versions.Create(ctx, &models.PostVersion{
			ID:            versionID,
			PostID:        req.PostID,
			VersionNumber: nextNum,
			Content:       result.UpdatedContent,
			Note:          result.VersionNote,
			Creator:       "assistant",
		}); err != nil {
			if isPostRemovedFKViolation(err) {
				slog.WarnContext(ctx, "post deleted mid-turn; discarding assistant result", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID)
				return nil, ErrPostRemovedDuringTurn
			}
			return nil, fmt.Errorf("create version: %w", err)
		}
		slog.InfoContext(ctx, "created version", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "version", nextNum, "note", result.VersionNote)
	}

	// ── Update post content ──────────────────────────────────────────────────
	if result.Action == "edited" {
		post.Content = result.UpdatedContent
		post.UpdatedAt = time.Now().UTC()
		if err := repos.Posts.Update(ctx, post); err != nil {
			return nil, fmt.Errorf("update post: %w", err)
		}
	}

	slog.InfoContext(ctx, "done", logging.AttrComponent, "genkit.post_assistant", "post_id", req.PostID, "duration_ms", time.Since(start).Milliseconds(), "action", result.Action, "save_version", result.SaveVersion)

	// Surface the clone before the canonical "complete" so the UI can
	// link to the new draft as soon as it exists.
	if result.CloneResult != nil {
		emit(onEvent, SSEEventCloneComplete, CloneCompleteEventPayload{
			NewPostID:  result.CloneResult.NewPostID,
			PlatformID: result.CloneResult.PlatformID,
			PostType:   result.CloneResult.PostType,
			Adapted:    result.CloneResult.Adapted,
		})
	}

	// Surface the restore outcome before the canonical "complete" so the
	// UI can refresh the version list / editor as soon as it lands.
	if result.RestoreResult != nil {
		emit(onEvent, SSEEventRestoreComplete, RestoreCompleteEventPayload{
			RestoredFromVersion: result.RestoreResult.RestoredFromVersion,
			NewVersionNumber:    result.RestoreResult.NewVersionNumber,
			NoOp:                result.RestoreResult.NoOp,
		})
	}

	// Surface the schedule outcome before the canonical "complete" so the
	// UI can refresh the post's status / scheduled time as soon as it lands.
	if result.ScheduleResult != nil {
		emit(onEvent, SSEEventScheduleComplete, ScheduleCompleteEventPayload{
			ScheduledAt: result.ScheduleResult.ScheduledAt,
			Status:      result.ScheduleResult.Status,
			AutoPublish: result.ScheduleResult.AutoPublish,
			Promoted:    result.ScheduleResult.Promoted,
		})
	}

	// Emit the final structured response. The client treats this as the
	// canonical result; delta events before this are preview-only.
	emit(onEvent, SSEEventComplete, &result)

	return &result, nil
}
