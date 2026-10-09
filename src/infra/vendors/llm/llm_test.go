package llm_test

import (
	"testing"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

func TestVendorsRegistered(t *testing.T) {
	for _, name := range []string{llm.VendorAnthropic, llm.VendorGemini} {
		d, ok := vendors.Get(name)
		if !ok {
			t.Fatalf("vendor %q not registered", name)
		}
		if d.Family != vendors.FamilyModel {
			t.Errorf("vendor %q family = %q, want model", name, d.Family)
		}
		if !d.Metered || d.Meter == nil {
			t.Errorf("vendor %q should be metered with a meter", name)
		}
	}
}

func TestAnthropicPricing(t *testing.T) {
	// 1M input @ $3/1M = 3_000_000 micros; 1M output @ $15/1M = 15_000_000.
	cost, ver, ok := vendors.CostOf(llm.VendorAnthropic, "claude-sonnet-4-5-20250929",
		vendors.Usage{vendors.KindInput: 1_000_000, vendors.KindOutput: 1_000_000})
	if !ok {
		t.Fatal("sonnet-4-5 not priced")
	}
	if cost != 18_000_000 {
		t.Errorf("cost = %d, want 18000000", cost)
	}
	if ver == "" {
		t.Error("price version empty")
	}

	// Haiku cache-read: 1M @ $0.10/1M = 100_000 micros.
	cost, _, ok = vendors.CostOf(llm.VendorAnthropic, "claude-haiku-4-5-20251001",
		vendors.Usage{vendors.KindCacheRead: 1_000_000})
	if !ok || cost != 100_000 {
		t.Errorf("haiku cache-read cost = %d, want 100000", cost)
	}
}

func TestHaiku55PricingByPromptLength(t *testing.T) {
	tests := []struct {
		name string
		u    vendors.Usage
		want int64
	}{
		{
			// 100K input @ $0.10/1M + 1M output @ $0.50/1M.
			name: "prompt at the threshold",
			u:    vendors.Usage{vendors.KindInput: 100_000, vendors.KindOutput: 1_000_000},
			want: 10_000 + 500_000,
		},
		{
			// Same request one token over: everything at $0.50/$2.50.
			name: "prompt over the threshold",
			u:    vendors.Usage{vendors.KindInput: 100_001, vendors.KindOutput: 1_000_000},
			want: 50_000 + 2_500_000,
		},
		{
			// 20K fresh + 90K cache-read is a 110K prompt: $0.50 input, $0.05 cache-read.
			name: "cache reads push the prompt over",
			u:    vendors.Usage{vendors.KindInput: 20_000, vendors.KindCacheRead: 90_000},
			want: 10_000 + 4_500,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cost, _, ok := vendors.CostOf(llm.VendorAnthropic, "claude-haiku-5-5", tc.u)
			if !ok || cost != tc.want {
				t.Fatalf("cost = %d (priced %v), want %d", cost, ok, tc.want)
			}
		})
	}
}

func TestClaude5CacheReadRates(t *testing.T) {
	// 1M cache-read tokens per model: 0.1× input, except Sonnet/Opus 5.5
	// (0.05×) and Fable 5.1 (0.025×).
	want := map[string]int64{
		"claude-sonnet-5":   200_000,
		"claude-sonnet-5-5": 100_000,
		"claude-opus-5":     500_000,
		"claude-opus-5-5":   200_000,
		"claude-fable-5-1":  250_000,
	}
	for model, micros := range want {
		cost, _, ok := vendors.CostOf(llm.VendorAnthropic, model, vendors.Usage{vendors.KindCacheRead: 1_000_000})
		if !ok || cost != micros {
			t.Errorf("%s cache-read cost = %d (priced %v), want %d", model, cost, ok, micros)
		}
	}
}

func TestGeminiEmbedPricing(t *testing.T) {
	// 1M embed input @ $0.15/1M = 150_000 micros.
	cost, _, ok := vendors.CostOf(llm.VendorGemini, "gemini-embedding-2",
		vendors.Usage{vendors.KindEmbedInput: 1_000_000})
	if !ok || cost != 150_000 {
		t.Errorf("gemini embed cost = %d, want 150000", cost)
	}
}

func TestGeminiFlashPricing(t *testing.T) {
	// 1M input @ $0.75/1M = 750_000 micros; 1M output @ $3.75/1M = 3_750_000.
	cost, _, ok := vendors.CostOf(llm.VendorGemini, "gemini-3.8-flash",
		vendors.Usage{vendors.KindInput: 1_000_000, vendors.KindOutput: 1_000_000})
	if !ok || cost != 4_500_000 {
		t.Errorf("gemini-3.8-flash cost = %d (priced %v), want 4500000", cost, ok)
	}
}

func TestGenkitGenerateMeter(t *testing.T) {
	d, _ := vendors.Get(llm.VendorAnthropic)
	resp := &ai.ModelResponse{Usage: &ai.GenerationUsage{
		InputTokens:         1000,
		OutputTokens:        500,
		CachedContentTokens: 200,
	}}
	op, u, ok := d.Meter.Extract(resp)
	if !ok || op != "generate" {
		t.Fatalf("Extract = (%q, _, %v), want (generate, _, true)", op, ok)
	}
	if u[vendors.KindInput] != 1000 || u[vendors.KindOutput] != 500 || u[vendors.KindCacheRead] != 200 {
		t.Errorf("usage = %v", u)
	}
	// cache_creation is unavailable through genkit — must be absent (0).
	if _, present := u[vendors.KindCacheCreation]; present {
		t.Error("cache_creation should not be set (genkit drops it)")
	}
}

func TestGenkitGenerateMeter_NoUsage(t *testing.T) {
	d, _ := vendors.Get(llm.VendorAnthropic)
	if _, _, ok := d.Meter.Extract(&ai.ModelResponse{}); ok {
		t.Error("nil Usage should yield ok=false")
	}
	if _, _, ok := d.Meter.Extract("not a response"); ok {
		t.Error("wrong type should yield ok=false")
	}
}

func TestEmbedMeter(t *testing.T) {
	d, _ := vendors.Get(llm.VendorGemini)
	op, u, ok := d.Meter.Extract(llm.EmbedUsage{Tokens: 4096})
	if !ok || op != "embed" || u[vendors.KindEmbedInput] != 4096 {
		t.Fatalf("Extract = (%q, %v, %v), want (embed, {embed_input:4096}, true)", op, u, ok)
	}
	if _, _, ok := d.Meter.Extract(llm.EmbedUsage{Tokens: 0}); ok {
		t.Error("zero tokens should yield ok=false")
	}
}
