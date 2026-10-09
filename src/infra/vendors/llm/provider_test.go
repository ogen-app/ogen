package llm_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// fakeModel answers every call with text and the given finish reason.
func fakeModel(t *testing.T, reason ai.FinishReason) *genkit.Genkit {
	t.Helper()
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/fake", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(context.Context, *ai.ModelRequest, ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			return &ai.ModelResponse{Message: ai.NewModelTextMessage("partial"), FinishReason: reason}, nil
		})
	return g
}

// TestCallConfig_Claude5Compatible pins the request shape Claude 5.x accepts:
// sampling parameters other than their defaults and a trailing assistant turn
// (prefill) are 400s there, so CallConfig must set neither.
func TestCallConfig_Claude5Compatible(t *testing.T) {
	var got *ai.ModelRequest
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/capture", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true, SystemRole: true}},
		func(_ context.Context, req *ai.ModelRequest, _ ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			got = req
			return &ai.ModelResponse{Message: ai.NewModelTextMessage("ok"), FinishReason: ai.FinishReasonStop}, nil
		})
	if _, err := genkit.Generate(t.Context(), g,
		ai.WithModelName("test/capture"),
		ai.WithSystem("system"),
		ai.WithMessages(ai.NewUserTextMessage("earlier"), ai.NewModelTextMessage("reply")),
		ai.WithPrompt("now"),
		llm.NewProvider().CallConfig(1000),
	); err != nil {
		t.Fatal(err)
	}

	cfg, ok := got.Config.(anthropic.MessageNewParams)
	if !ok {
		t.Fatalf("config type = %T, want anthropic.MessageNewParams", got.Config)
	}
	if want := (anthropic.MessageNewParams{MaxTokens: 1000}); !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v, want only MaxTokens set", cfg)
	}
	if last := got.Messages[len(got.Messages)-1]; last.Role != ai.RoleUser {
		t.Errorf("last message role = %q, want user (no prefill)", last.Role)
	}
}

func TestRefusalGuard(t *testing.T) {
	p := llm.NewProvider()
	tests := []struct {
		name    string
		reason  ai.FinishReason
		wantErr error
	}{
		{name: "refusal fails the call", reason: ai.FinishReasonUnknown, wantErr: llm.ErrRefused},
		{name: "normal stop passes", reason: ai.FinishReasonStop},
		{name: "truncation passes to the flow's own handling", reason: ai.FinishReasonLength},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeModel(t, tc.reason)
			resp, err := genkit.Generate(t.Context(), g,
				ai.WithModelName("test/fake"),
				ai.WithPrompt("go"),
				ai.WithMiddleware(p.RefusalGuard("test_flow")),
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && resp.Text() != "partial" {
				t.Fatalf("text = %q, want the model's answer", resp.Text())
			}
		})
	}
}
