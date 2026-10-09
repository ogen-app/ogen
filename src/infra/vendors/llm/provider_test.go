package llm_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// fakeModel answers every call with text, the given finish reason and 1000
// input / 10 output tokens. A positive cacheWrites is reported to the call's
// CacheWriteProbe, standing in for the HTTP transport reading the raw response.
func fakeModel(t *testing.T, reason ai.FinishReason, cacheWrites int64) *genkit.Genkit {
	t.Helper()
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/fake", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(ctx context.Context, _ *ai.ModelRequest, _ ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			if probe := llm.CacheWriteProbeFrom(ctx); probe != nil && cacheWrites > 0 {
				probe.Observe(cacheWrites)
			}
			return &ai.ModelResponse{
				Message:      ai.NewModelTextMessage("partial"),
				FinishReason: reason,
				Usage:        &ai.GenerationUsage{InputTokens: 1000, OutputTokens: 10},
			}, nil
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

func TestCallMiddleware_Refusal(t *testing.T) {
	p := llm.NewProvider()
	tests := []struct {
		name       string
		reason     ai.FinishReason
		wantErr    error
		wantRecord bool
	}{
		{name: "refusal fails the call and records its usage", reason: ai.FinishReasonUnknown, wantErr: llm.ErrRefused, wantRecord: true},
		{name: "normal stop passes, flow records it", reason: ai.FinishReasonStop},
		{name: "truncation passes to the flow's own handling", reason: ai.FinishReasonLength},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recorded []*ai.ModelResponse
			record := func(_ context.Context, r *ai.ModelResponse) { recorded = append(recorded, r) }
			g := fakeModel(t, tc.reason, 0)
			resp, err := genkit.Generate(t.Context(), g,
				ai.WithModelName("test/fake"),
				ai.WithPrompt("go"),
				ai.WithMiddleware(p.CallMiddleware("test_flow", record)),
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && resp.Text() != "partial" {
				t.Fatalf("text = %q, want the model's answer", resp.Text())
			}
			if got := len(recorded) == 1; got != tc.wantRecord {
				t.Fatalf("recorded %d responses, want record=%v", len(recorded), tc.wantRecord)
			}
			if tc.wantRecord && recorded[0].Usage.InputTokens != 1000 {
				t.Fatalf("recorded usage = %+v, want the refused call's tokens", recorded[0].Usage)
			}
		})
	}
}

// TestCallMiddleware_CacheWritesReachThePrice runs the metered path end to end:
// cache writes the transport reports are carried on the response, extracted by
// the Anthropic meter, and count toward Haiku 5.5's long-prompt threshold.
func TestCallMiddleware_CacheWritesReachThePrice(t *testing.T) {
	g := fakeModel(t, ai.FinishReasonStop, 120_000)
	resp, err := genkit.Generate(t.Context(), g,
		ai.WithModelName("test/fake"),
		ai.WithPrompt("go"),
		ai.WithMiddleware(llm.NewProvider().CallMiddleware("test_flow", nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	d, _ := vendors.Get(llm.VendorAnthropic)
	_, u, ok := d.Meter.Extract(resp)
	if !ok || u[vendors.KindCacheCreation] != 120_000 {
		t.Fatalf("metered usage = %v, want cache_creation 120000", u)
	}
	// 1000 input + 120K cache-write is a 121K prompt: every kind at the
	// long-prompt rates ($0.50 input, $2.50 output, $0.625 cache write).
	cost, _, _ := vendors.CostOf(llm.VendorAnthropic, "claude-haiku-5-5", u)
	if want := int64(500 + 25 + 75_000); cost != want {
		t.Fatalf("cost = %d, want %d", cost, want)
	}
}

func TestCacheWriteProbe_KeepsLargest(t *testing.T) {
	_, p := llm.WithCacheWriteProbe(t.Context())
	for _, n := range []int64{300, 1200, 1200, 50} {
		p.Observe(n)
	}
	if p.Tokens() != 1200 {
		t.Fatalf("tokens = %d, want 1200", p.Tokens())
	}
}
