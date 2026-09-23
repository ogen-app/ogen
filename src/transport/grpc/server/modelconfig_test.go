package server

import (
	"context"
	"errors"
	"testing"

	modelconfigv1 "github.com/ogen-app/ogen/gen/modelconfig/v1"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/modelprobe"
)

// fakeProber records calls and returns a canned result, so TestSlotModel's
// static→live wiring is exercised without a real model call.
type fakeProber struct {
	sample  string
	latency int64
	err     error
	calls   int
}

func (f *fakeProber) Probe(_ context.Context, _, _, _ string) (string, int64, error) {
	f.calls++
	return f.sample, f.latency, f.err
}

// TestListFlowsAndModels covers the read-only catalogs: ListFlows returns the
// code catalog (incl. the orchestrated post_assistant with two slots) and
// ListModels(embed) returns only embed models, priced and capability-tagged.
func TestListFlowsAndModels(t *testing.T) {
	s := newModelConfigAdminService(nil, nil) // reads don't touch the repo/prober
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

// TestTestSlotModelWiring covers the static→live sequencing of TestSlotModel:
// a static failure short-circuits before any probe; a static pass runs the
// prober and maps ok / error / unsupported / nil-prober correctly.
func TestTestSlotModelWiring(t *testing.T) {
	ctx := context.Background()
	req := func(flow, slot, model string) *modelconfigv1.TestSlotModelRequest {
		return &modelconfigv1.TestSlotModelRequest{FlowKey: flow, SlotKey: slot, ModelId: model}
	}

	// Static failure (Gemini chat model into a chat slot) short-circuits.
	fp := &fakeProber{}
	resp, err := newModelConfigAdminService(nil, fp).TestSlotModel(ctx, req(modelconfig.FlowContentPlan, modelconfig.SlotMain, "gemini-2.5-flash"))
	if err != nil {
		t.Fatalf("TestSlotModel: %v", err)
	}
	if resp.GetPassed() || len(resp.GetUnmetRequirements()) == 0 {
		t.Fatalf("static failure should report unmet: %+v", resp)
	}
	if fp.calls != 0 {
		t.Fatalf("prober ran despite static failure (%d calls)", fp.calls)
	}

	// Static pass + live ok.
	okP := &fakeProber{sample: "ok", latency: 42}
	resp, _ = newModelConfigAdminService(nil, okP).TestSlotModel(ctx, req(modelconfig.FlowContentPlan, modelconfig.SlotMain, "claude-sonnet-4-5-20250929"))
	if !resp.GetPassed() || resp.GetSample() != "ok" || resp.GetLatencyMs() != 42 || okP.calls != 1 {
		t.Fatalf("live-pass mapping wrong: %+v (calls=%d)", resp, okP.calls)
	}

	// Static pass + live error → failed with the probe's detail.
	errP := &fakeProber{err: errors.New("vendor not live")}
	resp, _ = newModelConfigAdminService(nil, errP).TestSlotModel(ctx, req(modelconfig.FlowContentPlan, modelconfig.SlotMain, "claude-sonnet-4-5-20250929"))
	if resp.GetPassed() || resp.GetDetail() != "vendor not live" {
		t.Fatalf("live-fail mapping wrong: %+v", resp)
	}

	// Probe unsupported (embed) → static pass.
	unsupP := &fakeProber{err: modelprobe.ErrProbeUnsupported}
	resp, _ = newModelConfigAdminService(nil, unsupP).TestSlotModel(ctx, req(modelconfig.FlowEmbed, modelconfig.SlotMain, "gemini-embedding-2"))
	if !resp.GetPassed() {
		t.Fatalf("unsupported probe should keep static pass: %+v", resp)
	}

	// Nil prober → static-only pass.
	resp, _ = newModelConfigAdminService(nil, nil).TestSlotModel(ctx, req(modelconfig.FlowContentPlan, modelconfig.SlotMain, "claude-sonnet-4-5-20250929"))
	if !resp.GetPassed() {
		t.Fatalf("nil prober should static-pass: %+v", resp)
	}
}
