//go:build integration

package integration_test

import (
	"context"

	"github.com/firebase/genkit/go/genkit"
	. "github.com/onsi/gomega"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// newAssistant registers the assistant with packs on g, as the server does.
func newAssistant(g *genkit.Genkit, repos assistant.Repos, packs ...assistant.Pack) assistant.Runner {
	a, err := assistant.Init(g, assistant.FlowConfig{Provider: llm.NewProvider(), MaxOutputTokens: 4096, MaxTurns: 6}, repos, packs...)
	Expect(err).NotTo(HaveOccurred())
	return a.Run
}

// runIn sends one message in a thread from screen.
func runIn(ctx context.Context, run assistant.Runner, scope viewcontext.Scope, screen viewcontext.Screen, instruction string, onEvent assistant.OnEventFunc) (*assistant.Response, error) {
	view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: screen, Scope: scope}}
	return run(ctx, assistant.Request{Instruction: instruction, View: view}, onEvent)
}

// campaignTurn is one message in a campaign's thread.
type campaignTurn struct {
	CampaignID  string
	Instruction string
}

// campaignTurnResult is a turn as the campaign suite reads it: the campaign
// pack's result, with the assistant's reply and the offers it made. Action
// is the pack's, "proposed" when the turn only made offers, else "answered".
type campaignTurnResult struct {
	campaign_assistant.CampaignAssistantResponse
	Proposals []assistant.ProposalCard
}

// campaignCallback runs campaign-thread messages through run.
func campaignCallback(run assistant.Runner) func(context.Context, campaignTurn, campaign_assistant.OnEventFunc) (*campaignTurnResult, error) {
	return func(ctx context.Context, req campaignTurn, onEvent campaign_assistant.OnEventFunc) (*campaignTurnResult, error) {
		resp, err := runIn(ctx, run, viewcontext.Scope{Kind: models.AssistantScopeCampaign, ID: req.CampaignID}, "campaign.overview", req.Instruction,
			func(kind assistant.SSEEventKind, data any) {
				if onEvent != nil {
					onEvent(campaign_assistant.SSEEventKind(kind), data)
				}
			})
		if err != nil {
			return nil, err
		}
		out := &campaignTurnResult{Proposals: resp.Proposals}
		if r, ok := resp.Results[campaign_assistant.PackName].(*campaign_assistant.CampaignAssistantResponse); ok {
			out.CampaignAssistantResponse = *r
		}
		out.Explanation = resp.Explanation
		switch {
		case out.Action != "":
		case len(resp.Proposals) > 0:
			out.Action = "proposed"
		default:
			out.Action = "answered"
		}
		return out, nil
	}
}

// postTurn is one message in a post's thread.
type postTurn struct {
	PostID      string
	Instruction string
}

// postCallback runs post-thread messages through run, returning the post
// pack's result with the assistant's reply; Action is "answered" when the
// post pack did nothing.
func postCallback(run assistant.Runner) func(context.Context, postTurn, assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error) {
	return func(ctx context.Context, req postTurn, onEvent assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error) {
		resp, err := runIn(ctx, run, viewcontext.Scope{Kind: models.AssistantScopePost, ID: req.PostID}, "post", req.Instruction, onEvent)
		if err != nil {
			return nil, err
		}
		out := &post_assistant.PostAssistantResponse{Action: "answered"}
		if r, ok := resp.Results[post_assistant.PackName].(*post_assistant.PostAssistantResponse); ok {
			out = r
		}
		out.Explanation = resp.Explanation
		return out, nil
	}
}
