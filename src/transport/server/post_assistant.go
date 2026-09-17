package server

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// initPostAssistant registers the post assistant flow on the shared Genkit
// instance and returns a callback for the posts handler.
func initPostAssistant(
	g *genkit.Genkit,
	cfg *config.Config,
	provider *llm.Provider,
	recorder *usage.Recorder,
	checker *usage.Checker,
	embedder ai.Embedder,
	hub eventhub.Hub,
	notifier *notify.Service,
	repos post_assistant.PostAssistantRepos,
	cloneSvc *clone.Service,
	restoreSvc *restore.Service,
	scheduleSvc *schedule.Service,
	noteSvc *notes.Service,
) (func(ctx context.Context, req post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error), error) {
	flowCfg := post_assistant.PostAssistantFlowConfig{
		Provider:        provider,
		Recorder:        recorder,
		Checker:         checker,
		ModelID:         cfg.ModelID,
		MaxOutputTokens: cfg.MaxOutputTokens,
		Embedder:        embedder,
		Hub:             hub,
		Notifier:        notifier,
		CloneService:    cloneSvc,
		RestoreService:  restoreSvc,
		ScheduleService: scheduleSvc,
		NoteService:     noteSvc,
		// CON-128: hybrid model split — the planner loop runs on the planning
		// model and delegates copywriting to the Sonnet editPost write-tool.
		// Off reverts to the legacy single-Sonnet path.
		PlannerEnabled:         cfg.PostAssistantPlanner,
		PlannerMaxOutputTokens: cfg.PostAssistantPlannerMaxOutputTokens,
		// CON-112: pre-warm the strict-tool grammar cache at boot when we're also
		// stabilizing tool order (otherwise the warmed key wouldn't match).
		PrewarmTools: cfg.AnthropicStableToolOrder,
	}
	if err := post_assistant.InitPostAssistant(g, flowCfg, repos); err != nil {
		return nil, fmt.Errorf("init post assistant flow: %w", err)
	}

	return post_assistant.NewPostAssistantCallback(), nil
}
