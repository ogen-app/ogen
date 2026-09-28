package modelconfig

import (
	"slices"
	"testing"
)

// TestCatalogIntegrity guards the shape the resolver, reconcile, and gRPC
// validation all assume: every slot has a real capability, the embed slot is
// global-only with the 3072-dim requirement, and orchestrated flows expose their
// distinct slots.
func TestCatalogIntegrity(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Flows() {
		if f.Key == "" {
			t.Fatal("flow with empty key")
		}
		if len(f.Slots) == 0 {
			t.Fatalf("flow %s has no slots", f.Key)
		}
		for _, s := range f.Slots {
			key := f.Key + "/" + s.Key
			if s.Key == "" {
				t.Fatalf("%s: slot with empty key", f.Key)
			}
			if seen[key] {
				t.Fatalf("duplicate slot %s", key)
			}
			seen[key] = true
			switch s.Capability {
			case CapabilityChat:
				if !slices.Equal(s.Vendors, []string{VendorAnthropic}) {
					t.Fatalf("%s: chat slot vendors = %v, want anthropic only", key, s.Vendors)
				}
			case CapabilityVision, CapabilityTranscribe:
				if !slices.Equal(s.Vendors, []string{VendorGemini}) {
					t.Fatalf("%s: service slot vendors = %v, want gemini only", key, s.Vendors)
				}
			case CapabilityEmbed:
			default:
				t.Fatalf("%s: bad capability %q", key, s.Capability)
			}
		}
	}

	embed, ok := LookupSlot(FlowEmbed, SlotMain)
	if !ok || !embed.GlobalOnly || embed.Capability != CapabilityEmbed || embed.Requires.EmbedDims != 3072 {
		t.Fatalf("embed slot = %+v ok=%v, want global-only embed dims 3072", embed, ok)
	}

	if _, ok := LookupSlot(FlowPostAssistant, SlotPlanner); !ok {
		t.Fatal("missing post_assistant/planner")
	}
	if _, ok := LookupSlot(FlowPostAssistant, SlotWriter); !ok {
		t.Fatal("missing post_assistant/writer")
	}
	for _, s := range []string{SlotClassify, SlotExtract, SlotEscalate, SlotAltText} {
		if sl, ok := LookupSlot(FlowVision, s); !ok || sl.Capability != CapabilityVision || sl.GlobalOnly {
			t.Fatalf("vision/%s = %+v ok=%v, want per-tier vision slot", s, sl, ok)
		}
	}
	if sl, ok := LookupSlot(FlowTranscribe, SlotMain); !ok || sl.Capability != CapabilityTranscribe || sl.GlobalOnly {
		t.Fatalf("transcribe/main = %+v ok=%v, want per-tier transcribe slot", sl, ok)
	}
	if _, ok := LookupSlot("nope", "nope"); ok {
		t.Fatal("unknown (flow, slot) resolved")
	}

	if got := len(AllSlots()); got != len(seen) {
		t.Fatalf("AllSlots returned %d, want %d", got, len(seen))
	}
}

// TestSatisfies covers the §8a static match: a capable model meets a demanding
// slot; a weak model reports every unmet requirement; an embed model with the
// wrong dimensionality is rejected.
func TestSatisfies(t *testing.T) {
	chat := ModelCapabilities{Capability: CapabilityChat, Tools: true, StructuredOutput: true, Streaming: true, MaxOutputTokens: 64000, ContextWindow: 200000}
	if unmet := Satisfies(chat, Requirements{NeedsTools: true, NeedsStructuredOutput: true, NeedsStreaming: true, MinMaxOutputTokens: 64000}); len(unmet) != 0 {
		t.Fatalf("capable model unmet=%v, want none", unmet)
	}

	weak := ModelCapabilities{Capability: CapabilityChat, MaxOutputTokens: 8192}
	if unmet := Satisfies(weak, Requirements{NeedsTools: true, NeedsStructuredOutput: true, MinMaxOutputTokens: 64000}); len(unmet) != 3 {
		t.Fatalf("weak model unmet=%v, want 3 (tools, structured_output, max_output_tokens)", unmet)
	}

	embed := ModelCapabilities{Capability: CapabilityEmbed, EmbedDims: 1536}
	if unmet := Satisfies(embed, Requirements{EmbedDims: 3072}); len(unmet) != 1 {
		t.Fatalf("wrong-dim embed unmet=%v, want 1", unmet)
	}
}

// TestSupports covers the modality mapping: vision/transcribe slots take a chat
// model with the matching input, never a text-only chat or an embed model.
func TestSupports(t *testing.T) {
	text := ModelCapabilities{Capability: CapabilityChat}
	multimodal := ModelCapabilities{Capability: CapabilityChat, VisionInput: true, AudioInput: true}
	embed := ModelCapabilities{Capability: CapabilityEmbed, VisionInput: true}

	cases := []struct {
		name string
		caps ModelCapabilities
		want Capability
		ok   bool
	}{
		{"text chat → chat", text, CapabilityChat, true},
		{"text chat → vision", text, CapabilityVision, false},
		{"text chat → transcribe", text, CapabilityTranscribe, false},
		{"multimodal → vision", multimodal, CapabilityVision, true},
		{"multimodal → transcribe", multimodal, CapabilityTranscribe, true},
		{"multimodal → embed", multimodal, CapabilityEmbed, false},
		{"embed with vision flag → vision", embed, CapabilityVision, false},
		{"embed → embed", embed, CapabilityEmbed, true},
	}
	for _, c := range cases {
		if got := c.caps.Supports(c.want); got != c.ok {
			t.Errorf("%s: Supports = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestAllowsVendor(t *testing.T) {
	if !(Slot{}).AllowsVendor("anything") {
		t.Fatal("slot without an allowlist rejected a vendor")
	}
	s := Slot{Vendors: []string{VendorGemini}}
	if !s.AllowsVendor(VendorGemini) || s.AllowsVendor(VendorAnthropic) {
		t.Fatalf("gemini-only slot: gemini=%v anthropic=%v", s.AllowsVendor(VendorGemini), s.AllowsVendor(VendorAnthropic))
	}
}
