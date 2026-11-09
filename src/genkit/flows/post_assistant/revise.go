package post_assistant

import (
	"context"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
)

// Reviser rewrites one post by an instruction from outside its thread, as
// editPost would in it, and saves the result as a version, so the post's
// history can restore what it was. A submitted post is refused.
type Reviser func(ctx context.Context, postID, instruction, note string) (*EditPostOutput, error)

// NewReviser returns the post pack's writer as a Reviser, over the same
// config and repositories as NewPack.
func NewReviser(g *genkit.Genkit, cfg PostAssistantFlowConfig, repos PostAssistantRepos) (Reviser, error) {
	p, err := loadPrompts()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, postID, instruction, note string) (*EditPostOutput, error) {
		ctx, _, err := begin(ctx, g, cfg, repos, p, assistant.Turn{Args: map[string]any{"postId": postID}})
		if err != nil {
			return nil, err
		}
		// The copy lands in the post, not in the thread that asked for it.
		getRequestState(ctx).onEvent = nil
		out, err := toolEditPost(ctx, EditPostInput{Instruction: instruction, Mode: "edit", SaveVersion: true, VersionNote: note})
		if err != nil {
			return nil, err
		}
		if _, err := end(ctx); err != nil {
			return nil, err
		}
		return out, nil
	}, nil
}
