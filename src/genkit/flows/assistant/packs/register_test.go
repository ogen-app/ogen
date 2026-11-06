package packs_test

import (
	"context"
	"testing"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant/packs"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

type proposals struct {
	repository.AssistantProposalRepository
}
type ideaRepo struct{ repository.IdeaRepository }
type assetRepo struct{ repository.AssetRepository }
type campaignRepo struct{ repository.CampaignRepository }
type postRepo struct{ repository.PostRepository }
type linkRepo struct{ repository.PostIdeaRepository }
type brandRepo struct{ repository.BrandRepository }

// Every assistant registers its tools on the one genkit instance the server
// runs; a tool name used twice panics at boot. This registers them all the
// way the runtime does.
func TestEveryAssistantRegistersTogether(t *testing.T) {
	g := genkit.Init(t.Context())
	if err := post_assistant.InitPostAssistant(g, post_assistant.PostAssistantFlowConfig{}, post_assistant.PostAssistantRepos{}); err != nil {
		t.Fatal(err)
	}
	if err := campaign_assistant.InitCampaignAssistant(g, campaign_assistant.CampaignAssistantFlowConfig{}, campaign_assistant.CampaignAssistantRepos{}); err != nil {
		t.Fatal(err)
	}
	d := packs.Deps{
		Campaign: func(context.Context, campaign_assistant.CampaignAssistantRequest, campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
			return nil, nil
		},
		Ideas: ideas.New(ideaRepo{}, nil), IdeaRepo: ideaRepo{},
		Compose: compose.New(nil, ideaRepo{}, campaignRepo{}, nil, nil, nil),
		Assets:  assetRepo{}, Campaigns: campaignRepo{},
		Posts: postRepo{}, Links: linkRepo{}, Analytics: &analytics.Service{},
		Brand: brandRepo{}, Activity: report.New(nil, nil, nil),
		CheckBrief: func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
			return nil, nil
		},
		CheckPosts: func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
			return nil, nil
		},
	}
	all := packs.All(d)
	if len(all) != 7 {
		t.Fatalf("packs = %d, want all seven", len(all))
	}
	if _, err := assistant.Init(g, assistant.FlowConfig{}, assistant.Repos{Proposals: proposals{}}, all...); err != nil {
		t.Fatal(err)
	}
}
