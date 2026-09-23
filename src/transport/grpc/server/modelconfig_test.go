package server

import (
	"context"
	"testing"

	modelconfigv1 "github.com/ogen-app/ogen/gen/modelconfig/v1"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
)

// TestListFlowsAndModels covers the read-only catalogs: ListFlows returns the
// code catalog (incl. the orchestrated post_assistant with two slots) and
// ListModels(embed) returns only embed models, priced and capability-tagged.
func TestListFlowsAndModels(t *testing.T) {
	s := newModelConfigAdminService(nil) // reads don't touch the repo
	ctx := context.Background()

	fr, err := s.ListFlows(ctx, &modelconfigv1.ListFlowsRequest{})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	slots := map[string]int{}
	for _, f := range fr.GetFlows() {
		slots[f.GetKey()] = len(f.GetSlots())
	}
	if slots[modelconfig.FlowPostAssistant] != 2 {
		t.Fatalf("post_assistant slots = %d, want 2 (planner+writer)", slots[modelconfig.FlowPostAssistant])
	}
	if slots[modelconfig.FlowContentPlan] != 1 {
		t.Fatalf("content_plan slots = %d, want 1", slots[modelconfig.FlowContentPlan])
	}

	mr, err := s.ListModels(ctx, &modelconfigv1.ListModelsRequest{Capability: "embed"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(mr.GetModels()) == 0 {
		t.Fatal("ListModels(embed) returned nothing")
	}
	for _, m := range mr.GetModels() {
		if m.GetCapability() != "embed" {
			t.Errorf("capability filter leaked %s (%s)", m.GetId(), m.GetCapability())
		}
		if m.GetPriceVersion() == "" || len(m.GetRates()) == 0 {
			t.Errorf("model %s missing price version/rates", m.GetId())
		}
	}
}

// TestUnmetForSlot covers the §8a static match used by both SetSlotModel and
// TestSlotModel: capability family, the v1 Anthropic-only-chat rule, and the
// requirements matrix.
func TestUnmetForSlot(t *testing.T) {
	chat, ok := modelconfig.LookupSlot(modelconfig.FlowContentPlan, modelconfig.SlotMain)
	if !ok {
		t.Fatal("content_plan/main missing")
	}
	embed, ok := modelconfig.LookupSlot(modelconfig.FlowEmbed, modelconfig.SlotMain)
	if !ok {
		t.Fatal("embed/main missing")
	}

	if u := unmetForSlot(chat, "claude-sonnet-4-5-20250929"); len(u) != 0 {
		t.Errorf("sonnet→content_plan unmet=%v, want none", u)
	}
	if u := unmetForSlot(embed, "gemini-embedding-2"); len(u) != 0 {
		t.Errorf("gemini-embedding-2→embed unmet=%v, want none", u)
	}
	if u := unmetForSlot(chat, "gemini-embedding-2"); len(u) == 0 {
		t.Error("embed model accepted into a chat slot")
	}
	if u := unmetForSlot(chat, "gemini-2.5-flash"); len(u) == 0 {
		t.Error("non-Anthropic chat model accepted in v1")
	}
	if u := unmetForSlot(chat, "does-not-exist"); len(u) == 0 {
		t.Error("unknown model accepted")
	}
}
