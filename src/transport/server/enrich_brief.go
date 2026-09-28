package server

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// initEnrichBrief registers the enrichBrief flow on the shared Genkit
// instance and returns an SSE-capable callback for the campaigns handler.
//
// MaxOutputTokens is left at 0 so the flow uses its own small default
// (32768). A brief is short, so the generation flows' larger cap would only
// be a looser guard; the flow default is the documented choice.
func initEnrichBrief(
	g *genkit.Genkit,
	cfg *config.Config,
	provider *llm.Provider,
	recorder *usage.Recorder,
	checker *usage.Checker,
	repos enrich_brief.EnrichBriefRepos,
) (func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error), error) {
	flowCfg := enrich_brief.EnrichBriefFlowConfig{
		Provider: provider,
		Recorder: recorder,
		Checker:  checker,
	}
	if err := enrich_brief.InitEnrichBrief(g, flowCfg, repos); err != nil {
		return nil, fmt.Errorf("init enrich brief flow: %w", err)
	}

	return enrich_brief.NewEnrichBriefCallback(), nil
}
