package post_assistant

import (
	"context"
	"errors"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
)

// Reviser rewrites one post by an instruction from outside its thread, as
// editPost would in it, and saves the result as a version, so the post's
// history can restore what it was. A submitted post is refused. A rewrite
// whose version couldn't be saved is ErrVersionNotSaved, with the output:
// the post has it all the same.
type Reviser func(ctx context.Context, postID, instruction, note string) (*EditPostOutput, error)

// ErrVersionNotSaved is a rewrite that is on the post already but couldn't
// be saved as a version, so the post's history holds no copy of it.
var ErrVersionNotSaved = errors.New("the post was already rewritten, but saving the rewrite as a version failed")

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
		if r := getRequestState(ctx).editResult; r == nil || !r.SaveVersion {
			return out, ErrVersionNotSaved
		}
		return out, nil
	}, nil
}
