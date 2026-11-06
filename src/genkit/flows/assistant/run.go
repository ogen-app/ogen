package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
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
) (*Response, error) {
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
	history := flowkit.History(rows, func(m models.AssistantMessage) (string, string) {
		if m.Role == flowkit.RoleModel {
			return m.Role, modelTurn(m.Content)
		}
		return m.Role, m.Content
	})
	view, err := renderView(p, req.View, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("render view: %w", err)
	}

	ctx = withTurnState(ctx, &turnState{view: req.View})
	scanner, err := callModel(ctx, g, cfg, p, tools, history, view+req.Instruction, onEvent)
	if err != nil {
		return nil, err
	}
	result, err := assemble(ctx, scanner)
	if err != nil {
		return nil, err
	}

	// Nothing a turn does depends on its own history row, so a failed write
	// is logged, not returned.
	if err := persistTurn(ctx, repos, req, result); err != nil {
		slog.ErrorContext(ctx, "persist conversation turn failed", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
	emit(onEvent, SSEEventComplete, result)
	return result, nil
}

// callModel runs the orchestrator's tool loop, streaming the explanation and
// the tool events. The reply is read by the tolerant scanner rather than
// ai.WithOutputType, whose strict validator drops a reply on common drift.
func callModel(ctx context.Context, g *genkit.Genkit, cfg FlowConfig, p *prompts, tools *toolSet, history []*ai.Message, prompt string, onEvent OnEventFunc) (*jsonstream.Scanner, error) {
	maxTokens := cmp.Or(cfg.MaxOutputTokens, 4096)
	mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotOrchestrator)
	scanner := jsonstream.New([]string{"explanation"}, func(key, delta string) {
		if key == "explanation" {
			emit(onEvent, SSEEventExplanationDelta, DeltaEventPayload{Delta: delta})
		}
	})
	usage := flowkit.Usage{Recorder: cfg.Recorder, Model: mc, Feature: "assistant", Component: logComponent}
	resp, err := genkit.Generate(ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(p.system),
		ai.WithMessages(history...),
		ai.WithPrompt(prompt),
		ai.WithTools(tools.all()...),
		ai.WithMaxTurns(cmp.Or(cfg.MaxTurns, 4)),
		ai.WithStreaming(flowkit.StreamCallback(flowkit.StreamHandlers{
			OnText: scanner.Push,
			OnToolCall: func(tr *ai.ToolRequest) {
				emit(onEvent, SSEEventToolCall, ToolCallEventPayload{Name: tr.Name, Input: tr.Input, Ref: tr.Ref})
			},
			OnToolResult: func(tr *ai.ToolResponse) {
				emit(onEvent, SSEEventToolResult, ToolResultEventPayload{Name: tr.Name, Ref: tr.Ref, OK: true})
			},
		})),
		// The system prompt, tools and history repeat every round and turn;
		// the per-turn <view> rides in the last message, after the breakpoints.
		ai.WithMiddleware(usage.Meter(), cfg.Provider.CallMiddleware(modelconfig.FlowAssistant, usage.Record, llm.CachePrompt())),
		cfg.Provider.CallConfig(mc.Model, maxTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, logging.AttrError, err)
		return nil, &AIError{Msg: assistantkit.FriendlyError(err)}
	}
	usage.FinishMetered(ctx, resp, maxTokens)
	return scanner, nil
}

// assemble reads the reply envelope. A reply in plain prose, with no JSON,
// is taken as the answer.
func assemble(ctx context.Context, scanner *jsonstream.Scanner) (*Response, error) {
	vals := scanner.Values()
	r := &Response{}
	r.Explanation, _ = vals["explanation"].(string)
	r.Action, _ = vals["action"].(string)
	if r.Explanation == "" {
		raw := strings.TrimSpace(scanner.FullText())
		if raw == "" || strings.Contains(raw, "{") {
			slog.ErrorContext(ctx, "reply had no usable fields", logging.AttrComponent, logComponent, "raw_preview", logging.Preview(raw, 500))
			return nil, &AIError{Msg: "model response did not contain the expected fields"}
		}
		r.Explanation, r.Action = raw, actionAnswered
	}
	if r.Action != actionDeclined {
		r.Action = actionAnswered
	}
	return r, nil
}

// modelTurn replays a stored model turn as its envelope, without the
// server-side fields a later version may add.
func modelTurn(content string) string {
	var env struct {
		Explanation string `json:"explanation"`
		Action      string `json:"action"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return content
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
