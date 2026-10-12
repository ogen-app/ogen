package asset_analysis

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// The default output cap is past what the Anthropic SDK allows on a blocking
// call, so the generator has to stream.
func TestGeneratorStreams(t *testing.T) {
	if _, err := anthropic.CalculateNonStreamingTimeout(int(defaultMaxOutputTokens), anthropic.ModelClaudeSonnet4_5, nil); err == nil {
		t.Fatal("the default cap fits a blocking call; streaming may no longer be needed")
	}

	streamed := false
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/analyst", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true, SystemRole: true}},
		func(_ context.Context, _ *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			streamed = cb != nil
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(`{"summary":"s"}`), FinishReason: ai.FinishReasonStop}, nil
		})

	gen := genkitGenerator(g, FlowConfig{Provider: llm.NewProvider()})
	text, err := gen(t.Context(), modelCall{Model: modelconfig.Resolved{Ref: "test/analyst", Model: "analyst"}, System: "sys", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !streamed || text != `{"summary":"s"}` {
		t.Fatalf("streamed = %t, text = %q", streamed, text)
	}
}
