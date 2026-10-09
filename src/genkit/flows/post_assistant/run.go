package post_assistant

import (
	"cmp"
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
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const logComponent = "genkit.post_assistant"

// Action values of PostAssistantResponse.Action.
const (
	actionEdited    = "edited"
	actionDeclined  = "declined"
	actionCloned    = "cloned"
	actionRestored  = "restored"
	actionScheduled = "scheduled"
	actionNoted     = "noted"
)

// Envelope keys the model emits.
const (
	keyExplanation    = "explanation"
	keyUpdatedContent = "updatedContent"
)

// isPostRemovedFKViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503) on a post_id constraint. Every post-referencing
// write in this flow goes through a *_post_id_fkey constraint, so a
// concurrent post deletion trips exactly these.
func isPostRemovedFKViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok &&
		pgErr.Code == "23503" &&
		strings.HasSuffix(pgErr.ConstraintName, "_post_id_fkey")
}

// flowPrompts are the templates and static prompt text a turn runs with.
type flowPrompts struct {
	system, context    *template.Template
	writerInstructions string
}

// turn carries one assistant request through its phases.
type turn struct {
	g       *genkit.Genkit
	req     PostAssistantRequest
	cfg     PostAssistantFlowConfig
	repos   PostAssistantRepos
	onEvent OnEventFunc
	start   time.Time

	post    *models.Post
	actx    *assistantContext
	history []*ai.Message
	st      *requestState
	scanner *jsonstream.Scanner
	result  PostAssistantResponse
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
	t := &turn{g: g, req: req, cfg: cfg, repos: repos, onEvent: onEvent, start: time.Now()}
	slog.InfoContext(ctx, "starting", logging.AttrComponent, logComponent, "post_id", req.PostID, "instruction_len", len(req.Instruction))

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	// Finalisation is scoped to the post owner, so failures before the post
	// loads are not announced.
	defer func() {
		if t.post == nil || t.post.CreatedBy == "" {
			return
		}
		publishAssistantFinalised(cfg.Hub, req.PostID, t.post.CreatedBy, out, retErr)
		notifyAssistantFinalised(cfg.Notifier, t.post.TenantID, t.post.CreatedBy, req.PostID, out, retErr)
	}()

	ctx, err := t.prepare(ctx, flowPrompts{system: systemTmpl, context: contextTmpl, writerInstructions: writerInstructions})
	if err != nil {
		return nil, err
	}
	if err := t.callModel(ctx, t.loopParams(tools)); err != nil {
		return nil, err
	}
	t.decodeEnvelope()
	t.applyToolOutcomes()
	if err := t.reconcile(ctx); err != nil {
		return nil, err
	}
	if err := t.persistMessages(ctx); err != nil {
		return nil, err
	}
	if err := t.commitEdit(ctx); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "done", logging.AttrComponent, logComponent, "post_id", req.PostID, "duration_ms", time.Since(t.start).Milliseconds(), "action", t.result.Action, "save_version", t.result.SaveVersion)
	t.emitCompletion()
	return &t.result, nil
}

// prepare validates the request, loads the post and its context, and returns
// ctx carrying the per-request tool state.
func (t *turn) prepare(ctx context.Context, p flowPrompts) (context.Context, error) {
	if t.req.Instruction == "" {
		return ctx, &ValidationError{Msg: "instruction is required"}
	}
	post, err := t.repos.Posts.GetByID(ctx, t.req.PostID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ctx, &ValidationError{Msg: "post not found"}
		}
		return ctx, fmt.Errorf("load post: %w", err)
	}
	t.post = post

	if err := t.ensureInitialVersion(ctx); err != nil {
		return ctx, err
	}
	if err := t.loadContextAndHistory(ctx, p); err != nil {
		return ctx, err
	}
	t.st = t.newRequestState(ctx, p.writerInstructions)
	return withRequestState(ctx, t.st), nil
}

// ensureInitialVersion snapshots the user's content as version 1 before the
// assistant's first change, so it can always be restored.
func (t *turn) ensureInitialVersion(ctx context.Context) error {
	count, err := t.repos.Versions.CountByPostID(ctx, t.req.PostID)
	if err != nil {
		return fmt.Errorf("count versions: %w", err)
	}
	if count != 0 || t.post.Content == "" {
		return nil
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	if err := t.repos.Versions.Create(ctx, &models.PostVersion{
		ID:            id,
		PostID:        t.req.PostID,
		VersionNumber: 1,
		Content:       t.post.Content,
		Note:          "Initial version",
		Creator:       models.PostVersionCreatorUser,
	}); err != nil {
		return t.writeErr(ctx, err, isPostRemovedFKViolation(err), "create initial version")
	}
	slog.InfoContext(ctx, "created initial version snapshot", logging.AttrComponent, logComponent, "post_id", t.req.PostID)
	return nil
}

func (t *turn) loadContextAndHistory(ctx context.Context, p flowPrompts) error {
	var ctxErr, histErr error
	var msgs []models.PostAssistantMessage
	var wg sync.WaitGroup
	wg.Go(func() {
		t.actx, ctxErr = assembleContextCached(ctx, t.post, t.repos, p.system, p.context)
	})
	wg.Go(func() {
		msgs, histErr = t.repos.Messages.ListRecentByPostID(ctx, t.req.PostID, 10)
	})
	wg.Wait()

	if ctxErr != nil {
		return fmt.Errorf("assemble context: %w", ctxErr)
	}
	if histErr != nil {
		return fmt.Errorf("load history: %w", histErr)
	}
	t.history = flowkit.History(msgs, func(m models.PostAssistantMessage) (string, string) { return m.Role, m.Content })
	return nil
}

func (t *turn) newRequestState(ctx context.Context, writerInstructions string) *requestState {
	st := &requestState{
		postID:          t.req.PostID,
		postStatus:      t.post.Status,
		assetIDs:        t.post.UsedAssetIDs,
		repos:           t.repos,
		embedder:        t.cfg.Embedder,
		cloneSvc:        t.cfg.CloneService,
		restoreSvc:      t.cfg.RestoreService,
		scheduleSvc:     t.cfg.ScheduleService,
		noteSvc:         t.cfg.NoteService,
		actor:           t.post.CreatedBy,
		platforms:       t.loadPlatforms(ctx),
		onEvent:         t.onEvent,
		g:               t.g,
		provider:        t.cfg.Provider,
		recorder:        t.cfg.Recorder,
		writerMaxTokens: t.cfg.MaxOutputTokens,
	}
	// The writer sees the same context block as the planner, so it knows the
	// campaign voice and current content without a second read. An empty
	// writerSystem (legacy path) disables the writer helpers.
	if t.cfg.PlannerEnabled {
		st.writerSystem = writerInstructions + "\n\n" + t.actx.ContextBlock
	}
	return st
}

// loadPlatforms backs the clonePost tool's platform-name resolution. It is
// best-effort: a failure only disables cross-platform clones.
func (t *turn) loadPlatforms(ctx context.Context) []models.Platform {
	if t.repos.Platforms == nil {
		return nil
	}
	ps, err := t.repos.Platforms.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "load platforms failed, clone name-resolution degraded", logging.AttrComponent, logComponent, "post_id", t.req.PostID, logging.AttrError, err)
		return nil
	}
	return ps
}

// loopParams configures the orchestration call. In the hybrid path the loop
// routes on the planner slot with a small output cap and delegates writing
// to the editPost tool; the legacy path writes the post inline on the writer
// slot and needs the full output budget.
type loopParams struct {
	slot      string
	maxTokens int64
	maxTurns  int
	tools     []ai.ToolRef
	// watch lists the envelope fields streamed as deltas. The planner never
	// emits updatedContent; the editPost writer streams content itself.
	watch []string
}

func (t *turn) loopParams(tools *toolSet) loopParams {
	p := loopParams{
		slot:      modelconfig.SlotWriter,
		maxTokens: cmp.Or(t.cfg.MaxOutputTokens, 64000),
		maxTurns:  cmp.Or(t.cfg.MaxTurns, 8),
		tools:     []ai.ToolRef{tools.listAssets, tools.getAssetChunks, tools.searchAssetChunks, tools.getCurrentContent, tools.clonePost, tools.restoreVersion, tools.schedulePost, tools.createNote},
		watch:     []string{keyExplanation, keyUpdatedContent},
	}
	if t.cfg.PlannerEnabled {
		p.slot = modelconfig.SlotPlanner
		p.maxTokens = cmp.Or(t.cfg.PlannerMaxOutputTokens, 8192)
		p.tools = append(p.tools, tools.editPost)
		p.watch = []string{keyExplanation}
	}
	return p
}

// schedulingPrompt prefixes the instruction with the scheduling context
// (current time, timezone, status, readiness). It changes every turn, so it
// goes into the user turn rather than the cached system prefix.
func (t *turn) schedulingPrompt(ctx context.Context) string {
	if t.cfg.ScheduleService == nil {
		return t.req.Instruction
	}
	if sb := buildSchedulingContext(ctx, t.post, t.repos); sb != "" {
		return sb + "\n\n---\n\nUser instruction: " + t.req.Instruction
	}
	return t.req.Instruction
}

// callModel runs the tool loop, streaming envelope deltas and tool events.
// Streaming is required by Anthropic for long tool-using requests, and
// ai.WithOutputType is deliberately not used: genkit's strict schema
// validator discards the whole response on minor JSON drift, so the
// envelope is parsed tolerantly by the scanner instead.
func (t *turn) callModel(ctx context.Context, p loopParams) error {
	mc := modelconfig.Resolve(ctx, modelconfig.FlowPostAssistant, p.slot)
	t.scanner = jsonstream.New(p.watch, t.emitDelta)
	prompt := t.schedulingPrompt(ctx)

	// Meter records every round of the tool loop, not just the last.
	usage := flowkit.Usage{
		Recorder:  t.cfg.Recorder,
		Model:     mc,
		Feature:   "post_assistant",
		Component: logComponent,
		Attrs:     []any{"post_id", t.req.PostID},
	}
	resp, err := genkit.Generate(ctx, t.g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(t.actx.SystemPrompt+"\n\n"+t.actx.ContextBlock),
		ai.WithMessages(t.history...),
		ai.WithPrompt(prompt),
		ai.WithTools(p.tools...),
		ai.WithMaxTurns(p.maxTurns),
		ai.WithStreaming(flowkit.StreamCallback(t.streamHandlers())),
		ai.WithMiddleware(usage.Meter(), t.cfg.Provider.CallMiddleware(modelconfig.FlowPostAssistant, usage.Record)),
		t.cfg.Provider.CallConfig(mc.Model, p.maxTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "post_id", t.req.PostID, "duration_ms", time.Since(t.start).Milliseconds(), logging.AttrError, err)
		return &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}
	usage.FinishMetered(ctx, resp, p.maxTokens)
	return nil
}

func (t *turn) emitDelta(key, delta string) {
	switch key {
	case keyExplanation:
		emit(t.onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
	case keyUpdatedContent:
		emit(t.onEvent, SSEEventContentDelta, DeltaEventPayload{Delta: delta})
	}
}

func (t *turn) streamHandlers() flowkit.StreamHandlers {
	return flowkit.StreamHandlers{
		OnText: t.scanner.Push,
		OnToolCall: func(tr *ai.ToolRequest) {
			emit(t.onEvent, SSEEventToolCall, ToolCallEventPayload{Name: tr.Name, Input: tr.Input, Ref: tr.Ref})
		},
		OnToolResult: func(tr *ai.ToolResponse) {
			emit(t.onEvent, SSEEventToolResult, ToolResultEventPayload{Name: tr.Name, Ref: tr.Ref, OK: true})
		},
	}
}

// decodeEnvelope reads the model's envelope from the scanner's tolerant
// parse, which survives trailing commas, prose, raw newlines and truncation.
func (t *turn) decodeEnvelope() {
	vals := t.scanner.Values()
	r := &t.result
	r.Explanation, _ = vals[keyExplanation].(string)
	r.UpdatedContent, _ = vals[keyUpdatedContent].(string)
	r.Action, _ = vals["action"].(string)
	r.SaveVersion, _ = vals["saveVersion"].(bool)
	r.VersionNote, _ = vals["versionNote"].(string)
}

// applyToolOutcomes lets the tools that ran this turn override the model's
// envelope; their committed results are authoritative. Edit runs before notes
// so an edit-and-note turn stays "edited".
func (t *turn) applyToolOutcomes() {
	t.applyClone()
	t.applyRestore()
	t.applySchedule()
	t.applyEdit()
	t.applyNotes()
}

func (t *turn) applyClone() {
	cr := t.st.cloneResult
	if cr == nil {
		return
	}
	r := &t.result
	r.Action = actionCloned
	r.UpdatedContent = ""
	r.SaveVersion = false
	if r.Explanation == "" {
		r.Explanation = fmt.Sprintf("Cloned this post into a new draft (#%s).", cr.Post.ID)
	}
	r.CloneResult = &CloneResultPayload{
		NewPostID:  cr.Post.ID,
		PlatformID: cr.Post.PlatformID,
		PostType:   cr.ResolvedPostType,
		Adapted:    cr.Adapted,
	}
}

// applyRestore surfaces the restored content; the restore service already
// swapped the post content and appended the versions.
func (t *turn) applyRestore() {
	rr := t.st.restoreResult
	if rr == nil {
		return
	}
	r := &t.result
	r.Action = actionRestored
	r.UpdatedContent = rr.Post.Content
	r.SaveVersion = false
	if r.Explanation == "" {
		if rr.NoOp {
			r.Explanation = fmt.Sprintf("The post already matches version %d — nothing to restore.", rr.RestoredFromVersion)
		} else {
			r.Explanation = fmt.Sprintf("Restored the post to version %d (saved as version %d). Your previous content is kept in the history.", rr.RestoredFromVersion, rr.NewVersionNumber)
		}
	}
	r.RestoreResult = &RestoreResultPayload{
		RestoredFromVersion: rr.RestoredFromVersion,
		NewVersionNumber:    rr.NewVersionNumber,
		NoOp:                rr.NoOp,
	}
}

func (t *turn) applySchedule() {
	sr := t.st.scheduleResult
	if sr == nil {
		return
	}
	r := &t.result
	r.Action = actionScheduled
	r.UpdatedContent = ""
	r.SaveVersion = false
	if r.Explanation == "" {
		mode := "auto-publish"
		if !sr.AutoPublish {
			mode = "manual publishing"
		}
		r.Explanation = fmt.Sprintf("Scheduled this post for %s (%s).",
			sr.ScheduledAt.Format("Jan 2, 2006 15:04 MST"), mode)
	}
	r.ScheduleResult = &ScheduleResultPayload{
		ScheduledAt: sr.ScheduledAt.Format(time.RFC3339),
		Status:      string(sr.Status),
		AutoPublish: sr.AutoPublish,
		Promoted:    sr.Promoted,
	}
}

// applyEdit takes the editPost writer's content; the planner's envelope still
// supplies saveVersion and versionNote.
func (t *turn) applyEdit() {
	if t.st.editResult == nil {
		return
	}
	t.result.Action = actionEdited
	t.result.UpdatedContent = t.st.editResult.Content
}

// applyNotes attaches the notes createNote persisted this turn. Notes are
// additive, so an edit's content is never cleared here; the turn becomes
// "noted" only when nothing else happened. Guarding on empty content keeps a
// truncated edit-and-note turn (action missing) inferable as "edited" later.
func (t *turn) applyNotes() {
	created := t.st.noteResults
	if len(created) == 0 {
		return
	}
	r := &t.result
	r.NotesCreated = make([]NotePayload, 0, len(created))
	for _, n := range created {
		r.NotesCreated = append(r.NotesCreated, NotePayload{ID: n.ID, Type: string(n.Type), Title: n.Title, Body: n.Body})
	}
	if r.Action != actionEdited && r.UpdatedContent == "" && t.st.cloneResult == nil && t.st.restoreResult == nil && t.st.scheduleResult == nil {
		r.Action = actionNoted
		r.SaveVersion = false
	}
	if r.Explanation == "" {
		if len(created) == 1 {
			r.Explanation = "Saved a note."
		} else {
			r.Explanation = fmt.Sprintf("Saved %d notes.", len(created))
		}
	}
}

// reconcile applies the safety and recovery rules to the assembled result.
func (t *turn) reconcile(ctx context.Context) error {
	t.rejectPlannerInlineEdit(ctx)
	t.recoverProse(ctx)
	if err := t.recoverTruncation(ctx); err != nil {
		return err
	}
	t.lockSubmittedPost(ctx)
	return nil
}

// rejectPlannerInlineEdit enforces that in the hybrid path only the editPost
// writer produces post copy. An "edited" turn without it either applied no
// edit (an empty body would wipe the post) or leaked planner-written prose,
// so the content is dropped and the turn downgraded.
func (t *turn) rejectPlannerInlineEdit(ctx context.Context) {
	r := &t.result
	if !t.cfg.PlannerEnabled || r.Action != actionEdited || t.st.editResult != nil {
		return
	}
	slog.WarnContext(ctx, "planner claimed an edit without invoking editPost; discarding any inline content", logging.AttrComponent, logComponent, "post_id", t.req.PostID)
	r.UpdatedContent = ""
	if len(t.st.noteResults) > 0 {
		r.Action = actionNoted
	} else {
		r.Action = actionDeclined
	}
	r.SaveVersion = false
	if r.Explanation == "" {
		r.Explanation = "I couldn't apply that edit — could you rephrase what you'd like changed?"
	}
}

// recoverProse salvages a reply where the model ignored the JSON envelope
// and answered in plain prose, showing it as a declined turn.
func (t *turn) recoverProse(ctx context.Context) {
	r := &t.result
	if r.Explanation != "" || r.UpdatedContent != "" {
		return
	}
	raw := strings.TrimSpace(t.scanner.FullText())
	if raw != "" && !strings.Contains(raw, "{") {
		slog.WarnContext(ctx, "model emitted prose-only response, treating as informational/declined", logging.AttrComponent, logComponent, "post_id", t.req.PostID, "len", len(raw))
		r.Explanation = raw
		r.Action = actionDeclined
	}
}

// recoverTruncation fills in fields lost to a max_tokens cut. The envelope
// order is explanation → updatedContent → action → saveVersion → versionNote,
// so truncation drops the trailing metadata first; only a reply with neither
// explanation nor content is unusable.
func (t *turn) recoverTruncation(ctx context.Context) error {
	r := &t.result
	switch {
	case r.Explanation == "" && r.UpdatedContent == "":
		raw := t.scanner.FullText()
		slog.ErrorContext(ctx, "scanner found no usable fields", logging.AttrComponent, logComponent, "post_id", t.req.PostID, "len", len(raw), "raw_preview", logging.Preview(raw, 500))
		return &AIError{Msg: "model response did not contain the expected fields"}
	case r.Action == "" && r.UpdatedContent != "":
		slog.WarnContext(ctx, "action missing, inferring edited from non-empty updatedContent (likely max_tokens truncation)", logging.AttrComponent, logComponent, "post_id", t.req.PostID)
		r.Action = actionEdited
	}
	if r.Explanation == "" && r.UpdatedContent != "" {
		r.Explanation = "Updated post content."
	}
	return nil
}

// lockSubmittedPost refuses a content edit on a submitted post. The editPost
// tool already refuses, but the legacy path writes content without a tool.
func (t *turn) lockSubmittedPost(ctx context.Context) {
	r := &t.result
	if r.Action != actionEdited || !t.post.Status.IsSubmitted() {
		return
	}
	slog.WarnContext(ctx, "refused content edit on submitted post", logging.AttrComponent, logComponent, "post_id", t.req.PostID, "status", string(t.post.Status))
	r.Action = actionDeclined
	r.UpdatedContent = ""
	r.SaveVersion = false
	r.Explanation = "This post is " + string(t.post.Status) + ", so its content is locked and I can't change it. Unschedule it (or duplicate it into a new draft) if you'd like to make edits."
}

// persistMessages stores the instruction and the model's envelope. The model
// row omits updatedContent to keep history small; storing JSON lets the UI
// restore the action badges and gives the model its own prior output back.
func (t *turn) persistMessages(ctx context.Context) error {
	if err := t.createMessage(ctx, flowkit.RoleUser, t.req.Instruction, "persist user message"); err != nil {
		return err
	}
	historyJSON, err := json.Marshal(struct {
		Action      string `json:"action"`
		Explanation string `json:"explanation"`
		SaveVersion bool   `json:"saveVersion"`
		VersionNote string `json:"versionNote,omitempty"`
		NoteCount   int    `json:"noteCount,omitzero"`
	}{
		Action:      t.result.Action,
		Explanation: t.result.Explanation,
		SaveVersion: t.result.SaveVersion,
		VersionNote: t.result.VersionNote,
		NoteCount:   len(t.result.NotesCreated),
	})
	if err != nil {
		return fmt.Errorf("marshal model history: %w", err)
	}
	return t.createMessage(ctx, flowkit.RoleModel, string(historyJSON), "persist model message")
}

func (t *turn) createMessage(ctx context.Context, role, content, op string) error {
	id, err := models.NewID()
	if err != nil {
		return err
	}
	if err := t.repos.Messages.Create(ctx, &models.PostAssistantMessage{
		ID:      id,
		PostID:  t.req.PostID,
		Role:    role,
		Content: content,
	}); err != nil {
		return t.writeErr(ctx, err, isPostRemovedFKViolation(err), op)
	}
	return nil
}

// commitEdit snapshots a requested version and writes the edited content.
// Content is stored as Markdown; only the frontend converts to editor JSON.
func (t *turn) commitEdit(ctx context.Context) error {
	r := &t.result
	if r.Action != actionEdited {
		return nil
	}
	if r.SaveVersion {
		if err := t.createVersion(ctx); err != nil {
			return err
		}
	}
	t.post.Content = r.UpdatedContent
	t.post.UpdatedAt = time.Now().UTC()
	if err := t.repos.Posts.Update(ctx, t.post); err != nil {
		return fmt.Errorf("update post: %w", err)
	}
	return nil
}

func (t *turn) createVersion(ctx context.Context) error {
	id, err := models.NewID()
	if err != nil {
		return err
	}
	version := &models.PostVersion{
		ID:      id,
		PostID:  t.req.PostID,
		Content: t.result.UpdatedContent,
		Note:    t.result.VersionNote,
		Creator: models.PostVersionCreatorAssistant,
	}
	if err := t.repos.Versions.CreateNext(ctx, version); err != nil {
		// CreateNext locks the post row first, so a deleted post surfaces as
		// ErrNoRows rather than an FK violation.
		return t.writeErr(ctx, err, errors.Is(err, sql.ErrNoRows) || isPostRemovedFKViolation(err), "create version")
	}
	slog.InfoContext(ctx, "created version", logging.AttrComponent, logComponent, "post_id", t.req.PostID, "version", version.VersionNumber, "note", t.result.VersionNote)
	return nil
}

// writeErr maps a failed post-referencing write: when postRemoved, the post
// was deleted mid-turn and the result is discarded with
// ErrPostRemovedDuringTurn; otherwise err is wrapped with op.
func (t *turn) writeErr(ctx context.Context, err error, postRemoved bool, op string) error {
	if postRemoved {
		slog.WarnContext(ctx, "post deleted mid-turn; discarding assistant result", logging.AttrComponent, logComponent, "post_id", t.req.PostID)
		return ErrPostRemovedDuringTurn
	}
	return fmt.Errorf("%s: %w", op, err)
}

// emitCompletion surfaces tool outcomes before the canonical "complete" event
// so the UI can refresh as soon as they land; deltas before it are
// preview-only.
func (t *turn) emitCompletion() {
	r := &t.result
	if cr := r.CloneResult; cr != nil {
		emit(t.onEvent, SSEEventCloneComplete, CloneCompleteEventPayload{
			NewPostID:  cr.NewPostID,
			PlatformID: cr.PlatformID,
			PostType:   cr.PostType,
			Adapted:    cr.Adapted,
		})
	}
	if rr := r.RestoreResult; rr != nil {
		emit(t.onEvent, SSEEventRestoreComplete, RestoreCompleteEventPayload{
			RestoredFromVersion: rr.RestoredFromVersion,
			NewVersionNumber:    rr.NewVersionNumber,
			NoOp:                rr.NoOp,
		})
	}
	if sr := r.ScheduleResult; sr != nil {
		emit(t.onEvent, SSEEventScheduleComplete, ScheduleCompleteEventPayload{
			ScheduledAt: sr.ScheduledAt,
			Status:      sr.Status,
			AutoPublish: sr.AutoPublish,
			Promoted:    sr.Promoted,
		})
	}
	emit(t.onEvent, SSEEventComplete, r)
}
