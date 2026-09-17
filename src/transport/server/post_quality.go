package server

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// initPostQuality registers the assessPostQuality flow on the shared
// Genkit instance and returns an SSE-capable callback for the posts
// handler. The weight profiles come from config (empty → built-in
// defaults); a malformed override fails here so the misconfiguration
// surfaces at boot rather than at first request.
func initPostQuality(
	g *genkit.Genkit,
	cfg *config.Config,
	provider *llm.Provider,
	recorder *usage.Recorder,
	checker *usage.Checker,
	hub eventhub.Hub,
	notifier *notify.Service,
	repos post_quality.PostQualityRepos,
) (func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error), error) {
	weights, err := post_quality.WeightsFromJSON(cfg.QualityWeightProfiles)
	if err != nil {
		return nil, fmt.Errorf("post quality weight profiles: %w", err)
	}

	// MaxOutputTokens is left at 0 so the flow uses its own small default
	// (defaultMaxOutputTokens). The generation flows' 64000 cap (cfg.MaxOutputTokens)
	// would trip the Anthropic non-streaming guard — evaluate uses a blocking
	// GenerateData call, and a scoring response is only a few thousand tokens.
	flowCfg := post_quality.PostQualityFlowConfig{
		Provider: provider,
		Recorder: recorder,
		Checker:  checker,
		ModelID:  cfg.QualityModelID,
		Weights:  weights,
		Hub:      hub,
		Notifier: notifier,
	}
	if err := post_quality.InitPostQuality(g, flowCfg, repos); err != nil {
		return nil, fmt.Errorf("init post quality flow: %w", err)
	}

	return post_quality.NewPostQualityCallback(), nil
}
