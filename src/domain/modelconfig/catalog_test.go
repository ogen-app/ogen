package modelconfig

import "testing"

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
			if s.Capability != CapabilityChat && s.Capability != CapabilityEmbed {
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
