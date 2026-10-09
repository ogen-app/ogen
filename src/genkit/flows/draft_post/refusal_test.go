package draft_post

import (
	"context"
	"errors"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// A refused stream must not fall back to a blocking call: the retry would be
// refused, and billed, again.
func TestGenerate_RefusalSkipsFallback(t *testing.T) {
	calls := 0
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/refuser", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(context.Context, *ai.ModelRequest, ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			calls++
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(""), FinishReason: ai.FinishReasonUnknown}, nil
		})

	err := (&draftSink{}).generate(t.Context(), g, flowkit.Usage{},
		ai.WithModelName("test/refuser"),
		ai.WithPrompt("go"),
		ai.WithMiddleware(llm.NewProvider().CallMiddleware("draft_post", nil)),
	)
	if _, ok := errors.AsType[*AIError](err); !ok {
		t.Fatalf("err = %v, want *AIError", err)
	}
	if calls != 1 {
		t.Fatalf("model called %d times, want 1", calls)
	}
}
