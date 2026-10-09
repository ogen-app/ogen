package campaign_assistant

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
)

const logComponent = "genkit.campaign_assistant"

// turn carries one assistant request through its phases.
type turn struct {
	g       *genkit.Genkit
	req     CampaignAssistantRequest
	cfg     CampaignAssistantFlowConfig
	repos   CampaignAssistantRepos
	onEvent OnEventFunc
	timer   *phaseTimer

	campaign *models.Campaign
	actx     *assistantContext
	history  []*ai.Message
	st       *requestState
	scanner  *jsonstream.Scanner
	// cutShort is set when the tool loop hit MaxTurns; committed results are
	// still finalised rather than failing the turn.
	cutShort bool
	outcomes []outcome
	result   CampaignAssistantResponse
}

func runCampaignAssistant(
	ctx context.Context,
	g *genkit.Genkit,
	req CampaignAssistantRequest,
	cfg CampaignAssistantFlowConfig,
	repos CampaignAssistantRepos,
	systemTmpl, contextTmpl *template.Template,
	tools *toolSet,
	onEvent OnEventFunc,
) (out *CampaignAssistantResponse, retErr error) {
	t := &turn{g: g, req: req, cfg: cfg, repos: repos, onEvent: onEvent, timer: newPhaseTimer()}
	slog.InfoContext(ctx, "starting", logging.AttrComponent, logComponent, "campaign_id", req.CampaignID, "instruction_len", len(req.Instruction))

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	t.timer.lap("enforce")

	// Finalisation is scoped to the campaign owner, so failures before the
	// campaign loads are not announced.
	defer func() {
		c := t.campaign
		if c == nil || c.CreatedBy == "" {
			return
		}
		publishAssistantFinalised(cfg.Hub, req.CampaignID, c.CreatedBy, out, retErr)
		// Fires only when this run generated a plan.
		notifyContentPlanReady(cfg.Notifier, c.TenantID, c.CreatedBy, req.CampaignID, out, retErr)
		// Skips the content-plan success already covered above.
		notifyAssistantFinalised(cfg.Notifier, c.TenantID, c.CreatedBy, req.CampaignID, out, retErr)
	}()

	// One Brand library load serves this turn and every sub-flow its tools run.
	ctx = brandresolve.WithMemo(ctx)
	ctx, err := t.prepare(ctx, systemTmpl, contextTmpl)
	if err != nil {
		return nil, err
	}
	if err := t.callModel(ctx, tools); err != nil {
		return nil, err
	}
	if err := t.assembleResult(ctx); err != nil {
		return nil, err
	}

	// A history-write failure must not fail the turn: tool side effects have
	// already committed, so it is logged and the completion events still go out.
	persistStart := time.Now()
	if err := persistTurn(ctx, repos, req, &t.result); err != nil {
		slog.ErrorContext(ctx, "persist conversation turn failed", logging.AttrComponent, logComponent, "campaign_id", req.CampaignID, logging.AttrError, err)
	}
	t.timer.persistMs = time.Since(persistStart).Milliseconds()

	t.timer.log(ctx, req.CampaignID, t.result.Action)
	slog.InfoContext(ctx, "done", logging.AttrComponent, logComponent, "campaign_id", req.CampaignID, "duration_ms", t.timer.totalMs(), "action", t.result.Action)

	// Tool completions go out before the canonical "complete" so the UI can
	// refresh as soon as they land; deltas before it are preview-only.
	for _, o := range t.outcomes {
		emit(onEvent, o.event, o.payload)
	}
	emit(onEvent, SSEEventComplete, &t.result)
	return &t.result, nil
}

// prepare validates the request, loads the campaign (tenant-scoped, so another
// tenant's campaign reads as not found), its context and history, and returns
// ctx carrying the per-request tool state.
func (t *turn) prepare(ctx context.Context, systemTmpl, contextTmpl *template.Template) (context.Context, error) {
	if strings.TrimSpace(t.req.Instruction) == "" {
		return ctx, &ValidationError{Msg: "instruction is required"}
	}
	campaign, err := t.repos.Campaigns.GetByID(ctx, t.req.CampaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ctx, &ValidationError{Msg: "campaign not found"}
		}
		return ctx, fmt.Errorf("load campaign: %w", err)
	}
	t.campaign = campaign
	t.timer.lap("load")

	// The history doesn't depend on the brand, so it loads alongside it.
	type historyResult struct {
		msgs []models.CampaignAssistantMessage
		err  error
	}
	historyCh := make(chan historyResult, 1)
	go func() {
		msgs, err := t.repos.Messages.ListRecentByCampaignID(ctx, t.req.CampaignID, 10)
		historyCh <- historyResult{msgs, err}
	}()

	// Brand resolution fails open: the result still carries the legacy tone.
	brand, err := brandresolve.Resolve(ctx, t.repos.Brands, campaign, nil)
	if err != nil {
		slog.WarnContext(ctx, "brand resolve failed; using legacy tone",
			logging.AttrComponent, logComponent, "error", err)
	}
	t.actx, err = assembleContext(campaign, brand.PromptBlock(""), time.Now().UTC(), systemTmpl, contextTmpl)
	if err != nil {
		return ctx, fmt.Errorf("assemble context: %w", err)
	}
	t.timer.lap("context")

	hr := <-historyCh
	msgs, err := hr.msgs, hr.err
	if err != nil {
		return ctx, fmt.Errorf("load history: %w", err)
	}
	t.history = flowkit.History(msgs, func(m models.CampaignAssistantMessage) (string, string) {
		if m.Role == flowkit.RoleModel {
			return m.Role, plannerTurn(m.Content)
		}
		return m.Role, m.Content
	})
	t.timer.lap("history")

	t.st = &requestState{
		campaignID:       t.req.CampaignID,
		campaign:         campaign,
		repos:            t.repos,
		onEvent:          t.onEvent,
		instruction:      t.req.Instruction,
		embedder:         t.cfg.Embedder,
		contentPlan:      t.cfg.ContentPlan,
		enrichBrief:      t.cfg.EnrichBrief,
		overview:         t.cfg.Overview,
		generatePosts:    t.cfg.GeneratePosts,
		maxGeneratePosts: t.cfg.MaxGeneratePosts,
		draftPost:        t.cfg.DraftPost,
		maxDraftPosts:    t.cfg.MaxDraftPosts,
		checkBrief:       t.cfg.CheckBrief,
		checkPosts:       t.cfg.CheckPosts,
	}
	return withRequestState(ctx, t.st), nil
}

// callModel runs the planner tool loop on the orchestrator slot, streaming the
// explanation and tool events. The envelope is parsed by the tolerant scanner
// rather than ai.WithOutputType, whose strict validator drops the whole
// response on common JSON drift.
//
// A MaxTurns abort is recoverable: tools that ran have committed and the
// explanation is already in the scanner, so the turn is finalised from them.
// genkit returns a nil response then; the rounds that ran are still metered
// by the Meter middleware (the sub-flows record their own).
func (t *turn) callModel(ctx context.Context, tools *toolSet) error {
	maxTokens := cmp.Or(t.cfg.MaxOutputTokens, 8192)
	maxTurns := cmp.Or(t.cfg.MaxTurns, 4)
	mc := modelconfig.Resolve(ctx, modelconfig.FlowCampaignAssistant, modelconfig.SlotOrchestrator)
	t.scanner = jsonstream.New([]string{"explanation"}, func(key, delta string) {
		if key == "explanation" {
			emit(t.onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
		}
	})

	// The sub-flows record their own usage, so the planner's is not double
	// counted. Meter records every round of the tool loop.
	usage := flowkit.Usage{
		Recorder:  t.cfg.Recorder,
		Model:     mc,
		Feature:   "campaign_assistant",
		Component: logComponent,
		Attrs:     []any{"campaign_id", t.req.CampaignID},
	}

	t.timer.genStart = time.Now()
	resp, err := genkit.Generate(ctx, t.g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(t.actx.SystemPrompt+"\n\n"+t.actx.ContextBlock),
		ai.WithMessages(t.history...),
		ai.WithPrompt(t.req.Instruction),
		ai.WithTools(tools.all()...),
		ai.WithMaxTurns(maxTurns),
		ai.WithStreaming(flowkit.StreamCallback(t.streamHandlers())),
		// The routing loop resends the same tools, system prompt and history
		// every round and turn, so its prompt is cached.
		ai.WithMiddleware(usage.Meter(), t.cfg.Provider.CallMiddleware(modelconfig.FlowCampaignAssistant, usage.Record, llm.CachePrompt())),
		t.cfg.Provider.CallConfig(mc.Model, maxTokens),
	)
	t.timer.genMs = time.Since(t.timer.genStart).Milliseconds()
	if err != nil {
		if !isMaxTurnsExceeded(err) {
			slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "campaign_id", t.req.CampaignID, "duration_ms", t.timer.totalMs(), logging.AttrError, err)
			return &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
		}
		t.cutShort = true
		slog.WarnContext(ctx, "tool-call budget exhausted; finalising from committed results", logging.AttrComponent, logComponent, "campaign_id", t.req.CampaignID, "max_turns", maxTurns, "duration_ms", t.timer.totalMs())
	}
	usage.FinishMetered(ctx, resp, maxTokens)
	return nil
}

func (t *turn) streamHandlers() flowkit.StreamHandlers {
	return flowkit.StreamHandlers{
		OnChunk: t.timer.chunk,
		OnText:  t.scanner.Push,
		OnToolCall: func(tr *ai.ToolRequest) {
			t.timer.toolCalled(tr.Ref)
			emit(t.onEvent, SSEEventToolCall, ToolCallEventPayload{Name: tr.Name, Input: tr.Input, Ref: tr.Ref})
		},
		OnToolResult: func(tr *ai.ToolResponse) {
			t.timer.toolReturned(tr.Name, tr.Ref)
			emit(t.onEvent, SSEEventToolResult, ToolResultEventPayload{Name: tr.Name, Ref: tr.Ref, OK: true})
		},
	}
}

// assembleResult merges the model's envelope with the committed tool
// outcomes and applies the recovery rules.
func (t *turn) assembleResult(ctx context.Context) error {
	vals := t.scanner.Values()
	r := &t.result
	r.Explanation, _ = vals["explanation"].(string)
	r.Action, _ = vals["action"].(string)

	t.outcomes = t.st.outcomes()
	if action := turnAction(t.outcomes, t.st.writes); action != "" {
		r.Action = action
	}
	for _, o := range t.outcomes {
		o.attach(r)
		if r.Explanation == "" {
			r.Explanation = o.explanation
		}
	}

	if r.Explanation == "" && len(t.outcomes) == 0 {
		t.recoverProse(ctx)
	}
	if r.Explanation == "" {
		return t.unusable(ctx)
	}
	if r.Action == "" {
		r.Action = actionAnswered
	}
	return nil
}

// recoverProse salvages a reply where the model ignored the JSON envelope
// and answered in plain prose (common for informational questions).
func (t *turn) recoverProse(ctx context.Context) {
	raw := strings.TrimSpace(t.scanner.FullText())
	if raw == "" || strings.Contains(raw, "{") {
		return
	}
	slog.WarnContext(ctx, "model emitted prose-only response, treating as answered", logging.AttrComponent, logComponent, "campaign_id", t.req.CampaignID, "len", len(raw))
	t.result.Explanation = raw
	t.result.Action = actionAnswered
}

// unusable reports a turn that produced nothing. When it was cut short at
// MaxTurns the planner kept calling tools without answering, so the user
// gets an actionable nudge instead of the generic parse failure.
func (t *turn) unusable(ctx context.Context) error {
	raw := t.scanner.FullText()
	if t.cutShort {
		slog.WarnContext(ctx, "turn cut short with no committed result", logging.AttrComponent, logComponent, "campaign_id", t.req.CampaignID, "len", len(raw))
		return &AIError{Msg: "I couldn't complete that in a single step. Try splitting it into smaller requests, or rephrasing."}
	}
	slog.ErrorContext(ctx, "scanner found no usable fields", logging.AttrComponent, logComponent, "campaign_id", t.req.CampaignID, "len", len(raw), "raw_preview", logging.Preview(raw, 500))
	return &AIError{Msg: "model response did not contain the expected fields"}
}

// isMaxTurnsExceeded reports whether err is genkit's "exceeded maximum tool
// call iterations" abort. genkit exports no sentinel for it, so it is matched
// on the stable message substring.
func isMaxTurnsExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "maximum tool call iterations")
}

// persistTurn stores the user instruction verbatim and the model turn as the
// same JSON the "complete" event carries, in one transaction, so a turn never
// persists half-written. Tool results are summaries and review findings, never
// generated post or brief content.
func persistTurn(ctx context.Context, repos CampaignAssistantRepos, req CampaignAssistantRequest, result *CampaignAssistantResponse) error {
	userMsgID, err := models.NewID()
	if err != nil {
		return err
	}
	modelMsgID, err := models.NewID()
	if err != nil {
		return err
	}

	historyJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal model history: %w", err)
	}

	if err := repos.Messages.CreateBatch(ctx, []*models.CampaignAssistantMessage{
		{ID: userMsgID, CampaignID: req.CampaignID, Role: flowkit.RoleUser, Content: req.Instruction},
		{ID: modelMsgID, CampaignID: req.CampaignID, Role: flowkit.RoleModel, Content: string(historyJSON)},
	}); err != nil {
		return fmt.Errorf("persist conversation turn: %w", err)
	}
	return nil
}
