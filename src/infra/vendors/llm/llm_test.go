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

func TestGeminiEmbedPricing(t *testing.T) {
	// 1M embed input @ $0.15/1M = 150_000 micros.
	cost, _, ok := vendors.CostOf(llm.VendorGemini, "gemini-embedding-2",
		vendors.Usage{vendors.KindEmbedInput: 1_000_000})
	if !ok || cost != 150_000 {
		t.Errorf("gemini embed cost = %d, want 150000", cost)
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
