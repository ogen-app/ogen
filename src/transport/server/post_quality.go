package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/qualityauto"
)

// initPostQuality registers the assessPostQuality flow on the shared
// Genkit instance and returns its entry points. The weight profiles come from config (empty → built-in
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
	limiter *entitlements.Limiter,
	repos post_quality.PostQualityRepos,
) (postQualityFns, error) {
	weights, err := post_quality.WeightsFromJSON(cfg.QualityWeightProfiles)
	if err != nil {
		return postQualityFns{}, fmt.Errorf("post quality weight profiles: %w", err)
	}

	// MaxOutputTokens is left at 0 so the flow uses its own small default
	// (defaultMaxOutputTokens). The generation flows' 64000 cap (cfg.MaxOutputTokens)
	// would trip the Anthropic non-streaming guard — evaluate uses a blocking
	// GenerateData call, and a scoring response is only a few thousand tokens.
	flowCfg := post_quality.PostQualityFlowConfig{
		Provider: provider,
		Recorder: recorder,
		Checker:  checker,
		Weights:  weights,
		Hub:      hub,
		Notifier: notifier,
		Limiter:  limiter,
	}
	if err := post_quality.InitPostQuality(g, flowCfg, repos); err != nil {
		return postQualityFns{}, fmt.Errorf("init post quality flow: %w", err)
	}

	return postQualityFns{
		assess: post_quality.NewPostQualityCallback(),
		status: post_quality.NewStatusCallback(),
		auto:   post_quality.NewAutoAssessCallback(),
	}, nil
}

// postQualityFns are the post quality flow's entry points: a manual
// assessment, the stored assessment with its staleness, and an automatic
// assessment from a lifecycle checkpoint.
type postQualityFns struct {
	assess func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error)
	status func(ctx context.Context, postID string) (*post_quality.Status, error)
	auto   func(ctx context.Context, postID string, trigger models.QualityTrigger) (*post_quality.PostQualityResponse, error)
}

// autoAssessRunner adapts the runtime's automatic assessment to the sweep:
// a skip (cap, spend, invalid post) is final, while the flows being
// unavailable or the model failing is worth another attempt.
func autoAssessRunner(gk *genkitRuntime) qualityauto.Runner {
	return func(ctx context.Context, postID string, trigger models.QualityTrigger) (qualityauto.Outcome, error) {
		_, err := gk.AutoAssessPost(ctx, postID, trigger)
		return autoOutcome(err), err
	}
}

func autoOutcome(err error) qualityauto.Outcome {
	if err == nil {
		return qualityauto.Done
	}
	if _, ok := errors.AsType[*post_quality.SkippedError](err); ok {
		return qualityauto.Skipped
	}
	if _, ok := errors.AsType[*post_quality.ValidationError](err); ok {
		return qualityauto.Skipped
	}
	return qualityauto.Failed
}
