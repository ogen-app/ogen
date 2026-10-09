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
		llm.NewProvider().CallConfig("claude-haiku-5-5", 1000),
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

// sentMaxTokens returns the max_tokens CallConfig(model, asked) sends.
func sentMaxTokens(t *testing.T, model string, asked int64) int64 {
	t.Helper()
	var got int64
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/capture", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(_ context.Context, req *ai.ModelRequest, _ ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			got = req.Config.(anthropic.MessageNewParams).MaxTokens
			return &ai.ModelResponse{Message: ai.NewModelTextMessage("ok"), FinishReason: ai.FinishReasonStop}, nil
		})
	if _, err := genkit.Generate(t.Context(), g,
		ai.WithModelName("test/capture"), ai.WithPrompt("go"),
		llm.NewProvider().CallConfig(model, asked),
	); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCallConfig_ClampsToModelMaxOutput: a cap sized for 5.x (128K output)
// must still work for a tier on 4.x (64K), so CallConfig clamps per model.
func TestCallConfig_ClampsToModelMaxOutput(t *testing.T) {
	tests := []struct {
		model string
		asked int64
		want  int64
	}{
		{model: "claude-haiku-4-5-20251001", asked: 128_000, want: 64_000},
		{model: "claude-sonnet-4-5-20250929", asked: 128_000, want: 64_000},
		{model: "claude-haiku-5-5", asked: 128_000, want: 128_000},
		{model: "claude-sonnet-5-5", asked: 200_000, want: 128_000},
		{model: "claude-haiku-4-5-20251001", asked: 8192, want: 8192},
		{model: "not-registered", asked: 200_000, want: 200_000},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			if got := sentMaxTokens(t, tc.model, tc.asked); got != tc.want {
				t.Fatalf("max_tokens = %d, want %d", got, tc.want)
			}
		})
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
