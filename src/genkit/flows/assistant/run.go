package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const logComponent = "genkit.assistant"

// historyMessages is how many earlier messages of the thread a turn sees.
const historyMessages = 10

func emit(onEvent OnEventFunc, kind SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(kind, data)
	}
}

func run(
	ctx context.Context,
	g *genkit.Genkit,
	req Request,
	cfg FlowConfig,
	repos Repos,
	p *prompts,
	tools *toolSet,
	onEvent OnEventFunc,
) (_ *Response, retErr error) {
	if strings.TrimSpace(req.Instruction) == "" {
		return nil, &ValidationError{Msg: "instruction is required"}
	}
	if req.View == nil || req.View.View == nil {
		return nil, &ValidationError{Msg: "viewContext is required"}
	}
	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	scope := req.View.View.Scope
	slog.InfoContext(ctx, "starting", logging.AttrComponent, logComponent,
		"scope", scope.Kind, "scope_id", scope.ID, "screen", req.View.View.Screen, "instruction_len", len(req.Instruction))

	rows, err := repos.Messages.ListRecent(ctx, scope.Kind, scope.ID, historyMessages)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	history := flowkit.History(rows, historyTurn)
	view, err := renderView(p, req.View, tools.openProposals(ctx, scope), time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("render view: %w", err)
	}

	st := newTurnState(req.View, tools.packs.defaults(req.View.View.Screen, scope.Kind))
	st.turn.Instruction = req.Instruction
	st.emit = onEvent
	// An offer answered before the turn failed stays answered, so the client
	// hears it even without a "complete". Work the packs' tools committed
	// before a failed model call is reported too, so the client refreshes.
	finished := false
	defer func() {
		if retErr == nil {
			return
		}
		if !finished {
			tools.end(st)
		}
		for _, r := range st.resolvedList() {
			emit(onEvent, SSEEventProposalResolved, r)
		}
	}()
	ctx = tools.bind(withTurnState(ctx, st), st)
	if ctx, err = tools.begin(ctx, st, slices.Clone(st.loaded)); err != nil {
		return nil, err
	}
	call := func(ctx context.Context, done []*ai.Message) (*modelReply, error) {
		return callModel(ctx, g, cfg, p, tools, st.loadedList(), history, view+st.packPrompt()+req.Instruction, done, onEvent)
	}
	reply, err := call(ctx, nil)
	if err != nil {
		return nil, err
	}
	// loadPack interrupts the call; the turn starts again, once, with the
	// pack's tools.
	if reply.scanner == nil {
		if ctx, reply, err = restartWithPack(ctx, tools, st, call, reply.done, onEvent); err != nil {
			return nil, err
		}
	}
	scanner := reply.scanner
	result := assemble(ctx, scanner)
	// Out of tool rounds, what the model wrote was written before more
	// tool calls: the packs' account of the work done stands in for it.
	preliminary := *result
	if reply.exhausted {
		result.Explanation = ""
	}
	tools.finish(st, result)
	finished = true
	if result.Explanation == "" {
		result.Explanation, result.Action = preliminary.Explanation, preliminary.Action
	}
	if result.Explanation == "" {
		return nil, &AIError{Msg: unusableReply(ctx, scanner)}
	}

	// The turn's work is done by now, so a failed history write does not
	// fail it: an error would report work that happened as lost, and a retry
	// would redo it. The reply says it was not saved instead.
	if err := persistTurn(ctx, repos, req, result); err != nil {
		slog.ErrorContext(ctx, "persist conversation turn failed", logging.AttrComponent, logComponent, logging.AttrError, err)
		result.Unsaved = true
	}
	emitFinal(onEvent, st, result)
	return result, nil
}

// emitFinal streams a finished turn: its offers and answered offers, its UI
// actions, then complete. The screen moves only once the reply is final, so
// a restarted call never leaves half a plan on it.
func emitFinal(onEvent OnEventFunc, st *turnState, result *Response) {
	for _, c := range result.Proposals {
		emit(onEvent, SSEEventProposalCreated, c)
	}
	for _, r := range st.resolvedList() {
		emit(onEvent, SSEEventProposalResolved, r)
	}
	for _, a := range result.UIActions {
		emit(onEvent, SSEEventUIAction, a)
	}
	emit(onEvent, SSEEventComplete, result)
}

// restartWithPack begins the pack loadPack asked for and makes the turn's
// second and last model call with it, continuing from the tool rounds done
// ran so their work isn't done again.
func restartWithPack(ctx context.Context, tools *toolSet, st *turnState, call func(context.Context, []*ai.Message) (*modelReply, error), done []*ai.Message, onEvent OnEventFunc) (context.Context, *modelReply, error) {
	pack, ok := st.restart(tools.packs)
	if !ok {
		return nil, nil, &AIError{Msg: "the assistant stopped without replying"}
	}
	ctx, err := tools.begin(ctx, st, []string{pack})
	if err != nil {
		return nil, nil, err
	}
	if st.hasBegun(pack) {
		emit(onEvent, SSEEventPackLoaded, PackLoadedEventPayload{Pack: pack})
	}
	reply, err := call(ctx, done)
	if err != nil {
		return nil, nil, err
	}
	if reply.scanner == nil {
		return nil, nil, &AIError{Msg: "the assistant stopped without replying"}
	}
	return ctx, reply, nil
}

// modelReply is what one model call left.
type modelReply struct {
	// scanner reads the reply; nil when a tool interrupted the call
	// (loadPack).
	scanner *jsonstream.Scanner
	// done is the tool rounds an interrupted call ran, for the restarted
	// call to continue from.
	done []*ai.Message
	// exhausted is set when the call ran out of tool rounds, so whatever
	// it wrote came before more tool calls.
	exhausted bool
}

// callModel runs the orchestrator's tool loop on the core tools plus loaded,
// streaming the explanation and the tool events. The reply is read by the
// tolerant scanner rather than ai.WithOutputType, whose strict validator
// drops a reply on common drift. done is the tool rounds a call that was
// interrupted ran, replayed after the prompt.
func callModel(ctx context.Context, g *genkit.Genkit, cfg FlowConfig, p *prompts, tools *toolSet, loaded []string, history []*ai.Message, prompt string, done []*ai.Message, onEvent OnEventFunc) (*modelReply, error) {
	system, err := p.system(loaded)
	if err != nil {
		return nil, err
	}
	messages := []ai.GenerateOption{ai.WithMessages(history...), ai.WithPrompt(prompt)}
	if len(done) > 0 {
		msgs := append(slices.Clone(history), ai.NewUserTextMessage(prompt))
		messages = []ai.GenerateOption{ai.WithMessages(append(msgs, done...)...)}
	}
	// The last request the model got holds the rounds run so far.
	var last *ai.ModelRequest
	lastRequest := func(next ai.ModelFunc) ai.ModelFunc {
		return func(ctx context.Context, req *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			last = req
			return next(ctx, req, cb)
		}
	}
	maxTokens := cmp.Or(cfg.MaxOutputTokens, 4096)
	mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotOrchestrator)
	reply := &replyScanner{newScanner: func() *jsonstream.Scanner {
		return jsonstream.New([]string{"explanation"}, func(key, delta string) {
			if key == "explanation" {
				emit(onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
			}
		})
	}}
	reply.cur = reply.newScanner()
	usage := flowkit.Usage{Recorder: cfg.Recorder, Model: mc, Feature: "assistant", Component: logComponent}
	resp, err := genkit.Generate(ctx, g, append(messages,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(system),
		ai.WithTools(tools.tools(loaded)...),
		// The runner, not the prompt, holds each tool to its tier.
		ai.WithUse(tools.reg.Guard()),
		ai.WithMaxTurns(cmp.Or(cfg.MaxTurns, 4)),
		ai.WithStreaming(flowkit.StreamCallback(flowkit.StreamHandlers{
			OnRound: reply.round,
			OnText:  reply.push,
			OnToolCall: func(tr *ai.ToolRequest) {
				emit(onEvent, SSEEventToolCall, ToolCallEventPayload{Name: tr.Name, Input: tr.Input, Ref: tr.Ref})
			},
			OnToolResult: func(tr *ai.ToolResponse) {
				emit(onEvent, SSEEventToolResult, ToolResultEventPayload{Name: tr.Name, Ref: tr.Ref, OK: assistantkit.OutputOK(tr.Output)})
			},
		})),
		// The system prompt, tools and history repeat every round and turn;
		// the per-turn <view> rides in the last message, after the breakpoints.
		ai.WithMiddleware(lastRequest, usage.Meter(), cfg.Provider.CallMiddleware(modelconfig.FlowAssistant, usage.Record, llm.CachePrompt())),
		cfg.Provider.CallConfig(mc.Model, maxTokens),
	)...)
	// Running out of tool rounds is recoverable: the tools that ran have
	// committed, and the packs' results stand in for the missing reply.
	if isMaxTurnsExceeded(err) {
		slog.WarnContext(ctx, "tool rounds exhausted; finalising from the work done", logging.AttrComponent, logComponent)
		usage.FinishMetered(ctx, nil, maxTokens)
		return &modelReply{scanner: reply.cur, exhausted: true}, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, logging.AttrError, err)
		return nil, &AIError{Msg: assistantkit.FriendlyError(err)}
	}
	usage.FinishMetered(ctx, resp, maxTokens)
	if len(resp.Interrupts()) > 0 {
		return &modelReply{done: doneRounds(last, resp.Message)}, nil
	}
	return &modelReply{scanner: reply.cur}, nil
}

// doneRounds is the tool rounds a call interrupted by loadPack ran: the
// rounds after the user's message in last, the call's last request, and
// the calls of the interrupted round that finished, with their results. The
// interrupted calls and the round's text are left out.
func doneRounds(last *ai.ModelRequest, interrupted *ai.Message) []*ai.Message {
	var out []*ai.Message
	if last != nil {
		for i, m := range slices.Backward(last.Messages) {
			if m.Role == ai.RoleUser {
				out = append(out, last.Messages[i+1:]...)
				break
			}
		}
	}
	if interrupted == nil {
		return out
	}
	var calls, results []*ai.Part
	for _, part := range interrupted.Content {
		output, ok := part.Metadata["pendingOutput"]
		if !part.IsToolRequest() || !ok {
			continue
		}
		tr := part.ToolRequest
		calls = append(calls, ai.NewToolRequestPart(&ai.ToolRequest{Name: tr.Name, Ref: tr.Ref, Input: tr.Input}))
		results = append(results, ai.NewToolResponsePart(&ai.ToolResponse{Name: tr.Name, Ref: tr.Ref, Output: output}))
	}
	if len(calls) > 0 {
		out = append(out, ai.NewModelMessage(calls...), ai.NewMessage(ai.RoleTool, nil, results...))
	}
	return out
}

// replyScanner reads the reply from the last model round that wrote text: a
// round that goes on to call tools may write a preliminary envelope first.
// A round with no text keeps the one before it.
type replyScanner struct {
	cur        *jsonstream.Scanner
	newScanner func() *jsonstream.Scanner
	newRound   bool
}

// round marks the start of a model round.
func (r *replyScanner) round() { r.newRound = true }

// push feeds text, starting a fresh scanner if a new round's text replaces
// an earlier round's.
func (r *replyScanner) push(text string) {
	if r.newRound {
		r.newRound = false
		if r.cur.FullText() != "" {
			r.cur = r.newScanner()
		}
	}
	r.cur.Push(text)
}

// isMaxTurnsExceeded reports genkit's "exceeded maximum tool call
// iterations" abort, which has no sentinel.
func isMaxTurnsExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "maximum tool call iterations")
}

// assemble reads the reply envelope. A reply in plain prose is taken as the
// answer, braces and all; one that opens with "{" or yielded any key is a
// JSON envelope. A reply with no explanation leaves Explanation empty for
// the packs' fallback.
func assemble(ctx context.Context, scanner *jsonstream.Scanner) *Response {
	vals := scanner.Values()
	r := &Response{}
	r.Explanation, _ = vals["explanation"].(string)
	r.Action, _ = vals["action"].(string)
	if r.Explanation == "" {
		if raw := strings.TrimSpace(scanner.FullText()); raw != "" && len(vals) == 0 && !strings.HasPrefix(jsonstream.StripFences(raw), "{") {
			r.Explanation = raw
		}
	}
	if r.Action != actionDeclined {
		r.Action = actionAnswered
	}
	return r
}

// unusableReply logs a turn that left neither a reply nor work to report,
// and says so in words the user can act on.
func unusableReply(ctx context.Context, scanner *jsonstream.Scanner) string {
	raw := scanner.FullText()
	slog.ErrorContext(ctx, "reply had no usable fields", logging.AttrComponent, logComponent, "raw_preview", logging.Preview(raw, 500))
	if strings.TrimSpace(raw) == "" {
		return "I couldn't complete that in a single step. Try splitting it into smaller requests, or rephrasing."
	}
	return "model response did not contain the expected fields"
}

// historyTurn replays a stored message: the user's as typed, after a note of
// what was selected when it was sent; a model turn as its envelope.
func historyTurn(m models.AssistantMessage) (string, string) {
	if m.Role == flowkit.RoleModel {
		return m.Role, modelTurn(m.Content)
	}
	return m.Role, earlierView(m.ViewContext) + m.Content
}

// earlierIDs caps the ids an <earlier-view> note lists.
const earlierIDs = 5

// earlierView is a one-line note of the focus and selection a stored user
// message was sent with, or "" when it had neither. It is built from the
// stored row alone, so the replayed history stays a stable cache prefix.
func earlierView(stored models.JSONMap) string {
	if stored == nil {
		return ""
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return ""
	}
	var v viewcontext.ViewContext
	if err := json.Unmarshal(b, &v); err != nil || (v.Focus == nil && v.Selection == nil) {
		return ""
	}
	parts := []string{"sent from " + string(v.Screen)}
	if v.Focus != nil {
		parts = append(parts, fmt.Sprintf("in focus %s %s", v.Focus.Kind, v.Focus.ID))
	}
	switch sel := v.Selection; {
	case sel == nil:
	case sel.AllMatching:
		parts = append(parts, fmt.Sprintf("selected every %s matching %q", sel.Kind, v.Filter))
	case len(sel.IDs) > 0:
		ids := strings.Join(sel.IDs[:min(len(sel.IDs), earlierIDs)], ", ")
		if more := len(sel.IDs) - earlierIDs; more > 0 {
			ids += fmt.Sprintf(" and %d more", more)
		}
		parts = append(parts, fmt.Sprintf("selected %s %s", sel.Kind, ids))
	}
	return "<earlier-view>" + strings.Join(parts, "; ") + "</earlier-view>\n"
}

// modelTurn replays a stored model turn as its envelope, without the
// server-side fields a later version may add, naming the assets the turn
// drew on.
func modelTurn(content string) string {
	var env struct {
		Explanation string `json:"explanation"`
		Action      string `json:"action"`
		// Sources names the assets the answer drew on, so a later turn can
		// tell it came from the user's material without seeing the search.
		Sources []string `json:"sources,omitempty"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return content
	}
	for _, s := range assistantkit.Cited([]byte(content)) {
		env.Sources = append(env.Sources, cmp.Or(s.Title, s.ID))
	}
	out, err := json.Marshal(env)
	if err != nil {
		return content
	}
	return string(out)
}

// persistTurn stores the instruction with the screen it was sent from, and
// the model turn as the JSON the "complete" event carried, in one batch.
func persistTurn(ctx context.Context, repos Repos, req Request, result *Response) error {
	view, err := req.View.View.Stored()
	if err != nil {
		return err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal model turn: %w", err)
	}
	userID, _ := logging.UserIDFrom(ctx)
	scope := req.View.View.Scope
	now := time.Now().UTC()
	msgs := []*models.AssistantMessage{
		{ScopeKind: scope.Kind, ScopeID: scope.ID, Role: flowkit.RoleUser, Content: req.Instruction, ViewContext: view, CreatedBy: userID, CreatedAt: now},
		// The model turn sorts after the user's even at equal clock reads.
		{ScopeKind: scope.Kind, ScopeID: scope.ID, Role: flowkit.RoleModel, Content: string(body), CreatedBy: userID, CreatedAt: now.Add(time.Microsecond)},
	}
	for _, m := range msgs {
		if m.ID, err = models.NewID(); err != nil {
			return err
		}
	}
	return repos.Messages.CreateBatch(ctx, msgs)
}
