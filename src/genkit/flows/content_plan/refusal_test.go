package content_plan

import (
	"context"
	"errors"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// A refused stream must not fall back to a blocking call: the retry would be
// refused, and billed, again.
func TestStream_RefusalSkipsFallback(t *testing.T) {
	calls := 0
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/refuser", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(context.Context, *ai.ModelRequest, ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			calls++
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(""), FinishReason: ai.FinishReasonUnknown}, nil
		})
	gen := &postGenerator{
		g:         g,
		modelName: "test/refuser",
		modelOpts: []ai.GenerateOption{ai.WithMiddleware(llm.NewProvider().CallMiddleware("content_plan", nil))},
	}

	posts, err := gen.stream(t.Context(), "go", 0, 0, func(SSEEventKind, any) {})
	if _, ok := errors.AsType[*AIError](err); !ok {
		t.Fatalf("err = %v, want *AIError", err)
	}
	if len(posts) != 0 || calls != 1 {
		t.Fatalf("posts = %d, model calls = %d; want 0 posts and 1 call", len(posts), calls)
	}
}
