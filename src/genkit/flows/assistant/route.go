package assistant

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
)

// Routing is what the orchestrator does first with a message: the packs the
// turn starts with, and the tools it asks for, none of them run.
type Routing struct {
	Packs []string     `json:"packs"`
	Calls []RoutedCall `json:"calls"`
	// Reply is what it said instead, when it called no tool.
	Reply string `json:"reply,omitempty"`
}

// RoutedCall is one tool the orchestrator asked for.
type RoutedCall struct {
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
}

// route makes a turn's first model call as run does — the same history,
// <view>, pack blocks, system prompt and tools — but returns the tool calls
// instead of running them, so evals can check which tools a message reaches
// without writing anything. The packs still begin, which reads their subject.
func route(ctx context.Context, g *genkit.Genkit, req Request, cfg FlowConfig, repos Repos, p *prompts, tools *toolSet) (*Routing, error) {
	if req.View == nil || req.View.View == nil {
		return nil, &ValidationError{Msg: "viewContext is required"}
	}
	scope := req.View.View.Scope
	rows, err := repos.Messages.ListRecent(ctx, scope.Kind, scope.ID, historyMessages)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	view, err := renderView(p, req.View, tools.openProposals(ctx, scope), time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("render view: %w", err)
	}
	st := newTurnState(req.View, tools.packs.defaults(req.View.View.Screen, scope.Kind))
	st.turn.Instruction = req.Instruction
	ctx = tools.bind(withTurnState(ctx, st), st)
	if ctx, err = tools.begin(ctx, st, slices.Clone(st.loaded)); err != nil {
		return nil, err
	}
	loaded := st.loadedList()
	system, err := p.system(loaded)
	if err != nil {
		return nil, err
	}
	mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotOrchestrator)
	maxTokens := cmp.Or(cfg.MaxOutputTokens, 4096)
	resp, err := genkit.Generate(ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(system),
		ai.WithMessages(flowkit.History(rows, historyTurn)...),
		ai.WithPrompt(view+st.packPrompt()+req.Instruction),
		ai.WithTools(tools.tools(loaded)...),
		ai.WithReturnToolRequests(true),
		cfg.Provider.CallConfig(mc.Model, maxTokens),
	)
	if err != nil {
		return nil, &AIError{Msg: assistantkit.FriendlyError(err)}
	}
	out := &Routing{Packs: loaded, Calls: []RoutedCall{}}
	for _, tr := range resp.ToolRequests() {
		out.Calls = append(out.Calls, RoutedCall{Name: tr.Name, Input: tr.Input})
	}
	if len(out.Calls) == 0 {
		out.Reply = resp.Text()
	}
	return out, nil
}
