//go:build integration

package integration_test

import (
	"context"

	"github.com/firebase/genkit/go/genkit"
	. "github.com/onsi/gomega"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
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
