package server

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// initContentPlan registers the content plan flow on the shared Genkit
// instance and returns an SSE-capable callback for the campaigns handler.
func initContentPlan(
	g *genkit.Genkit,
	cfg *config.Config,
	provider *llm.Provider,
	recorder *usage.Recorder,
	checker *usage.Checker,
	embedder ai.Embedder,
	hub eventhub.Hub,
	notifier *notify.Service,
	repos content_plan.ContentPlanRepos,
) (func(ctx context.Context, campaignID string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error), error) {
	flowCfg := content_plan.ContentPlanFlowConfig{
		Provider:           provider,
		Recorder:           recorder,
		Checker:            checker,
		ModelID:            cfg.ModelID,
		MaxContextAssets:   cfg.MaxContextAssets,
		MaxContextChars:    cfg.MaxContextChars,
		MaxOutputTokens:    cfg.MaxOutputTokens,
		MaxPostsPerBatch:   cfg.MaxPostsPerBatch,
		MaxParallelBatches: cfg.MaxParallelBatches,
		Embedder:           embedder,
		Hub:                hub,
		Notifier:           notifier,
	}
	if err := content_plan.InitContentPlan(g, flowCfg, repos); err != nil {
		return nil, fmt.Errorf("init content plan flow: %w", err)
	}

	return content_plan.NewContentPlanCallback(), nil
}
