package server

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// initConsistency registers the read-only consistency review flow on
// the shared Genkit instance and returns the brief + posts review callbacks for
// the campaigns handler and the campaign assistant tools.
func initConsistency(
	g *genkit.Genkit,
	cfg *config.Config,
	provider *llm.Provider,
	recorder *usage.Recorder,
	checker *usage.Checker,
	hub eventhub.Hub,
	campaigns repository.CampaignRepository,
	posts repository.PostRepository,
) (
	func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error),
	func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error),
	error,
) {
	flowCfg := consistency.ConsistencyFlowConfig{
		Provider: provider,
		Recorder: recorder,
		Checker:  checker,
		ModelID:  cfg.ModelID,
		MaxPosts: cfg.ConsistencyPostsMax,
		Hub:      hub,
	}
	if err := consistency.InitConsistency(g, flowCfg, consistency.ConsistencyRepos{Campaigns: campaigns, Posts: posts}); err != nil {
		return nil, nil, fmt.Errorf("init consistency flow: %w", err)
	}
	return consistency.NewCheckBriefCallback(), consistency.NewCheckPostsCallback(), nil
}
