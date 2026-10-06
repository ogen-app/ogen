package server

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// TestListModelsOmitsNonAssignableChat proves ListModels drops chat-capable
// models that no chat slot could accept in v1 (non-Anthropic), while keeping the
// Anthropic chat models.
func TestListModelsOmitsNonAssignableChat(t *testing.T) {
	s := newModelConfigAdminService(nil, nil)
	resp, err := s.ListModels(context.Background(), &modelconfigv1.ListModelsRequest{Capability: "chat"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(resp.GetModels()) == 0 {
		t.Fatal("ListModels(chat) returned nothing")
	}
	sawAnthropic := false
	for _, m := range resp.GetModels() {
		if m.GetVendor() != "anthropic" {
			t.Errorf("chat list includes non-assignable %s (vendor %s)", m.GetId(), m.GetVendor())
		}
		if m.GetVendor() == "anthropic" {
			sawAnthropic = true
		}
	}
	if !sawAnthropic {
		t.Fatal("chat list has no Anthropic models")
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

	extract, ok := modelconfig.LookupSlot(modelconfig.FlowVision, modelconfig.SlotExtract)
	if !ok {
		t.Fatal("vision/extract missing")
	}
	transcribe, ok := modelconfig.LookupSlot(modelconfig.FlowTranscribe, modelconfig.SlotMain)
	if !ok {
		t.Fatal("transcribe/main missing")
	}
	if u := unmetForSlot(extract, "gemini-2.5-pro"); len(u) != 0 {
		t.Errorf("gemini-2.5-pro→vision/extract unmet=%v, want none", u)
	}
	if u := unmetForSlot(transcribe, "gemini-2.5-flash"); len(u) != 0 {
		t.Errorf("gemini-2.5-flash→transcribe unmet=%v, want none", u)
	}
	for _, sr := range modelconfig.AllSlots() {
		if sr.FlowKey != modelconfig.FlowVision && sr.FlowKey != modelconfig.FlowTranscribe {
			continue
		}
		if u := unmetForSlot(sr.Slot, "gemini-3.8-flash"); len(u) != 0 {
			t.Errorf("gemini-3.8-flash→%s/%s unmet=%v, want none", sr.FlowKey, sr.Slot.Key, u)
		}
	}
	if u := unmetForSlot(extract, "claude-sonnet-4-5-20250929"); len(u) == 0 {
		t.Error("Claude model accepted into a vision slot")
	}
	if u := unmetForSlot(transcribe, "gemini-embedding-2"); len(u) == 0 {
		t.Error("embed model accepted into the transcribe slot")
	}
}

// TestListModelsVision lists exactly the models a vision slot accepts: the
// multimodal Gemini models, not Claude and not the embedder.
func TestListModelsVision(t *testing.T) {
	resp, err := newModelConfigAdminService(nil, nil).ListModels(context.Background(), &modelconfigv1.ListModelsRequest{Capability: "vision"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	got := map[string]bool{}
	for _, m := range resp.GetModels() {
		got[m.GetId()] = true
		if m.GetVendor() != "gemini" {
			t.Errorf("vision list includes %s (vendor %s)", m.GetId(), m.GetVendor())
		}
	}
	if !got["gemini-2.5-flash"] || !got["gemini-2.5-pro"] || !got["gemini-3.8-flash"] {
		t.Fatalf("vision list = %v, want gemini-2.5-flash, gemini-2.5-pro and gemini-3.8-flash", got)
	}
	if got["gemini-embedding-2"] {
		t.Fatal("vision list includes the embedding model")
	}
}

// TestWireCarriesSlotAssignability plays Harbor: it loads the unfiltered model
// list and the flow catalog, then picks models per slot from wire fields alone.
// Every slot must get a non-empty picker, and every model offered must pass the
// server's own write-path check.
func TestWireCarriesSlotAssignability(t *testing.T) {
	s := newModelConfigAdminService(nil, nil)
	ctx := context.Background()
	fr, err := s.ListFlows(ctx, &modelconfigv1.ListFlowsRequest{})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	mr, err := s.ListModels(ctx, &modelconfigv1.ListModelsRequest{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	fits := func(m *modelconfigv1.Model, sl *modelconfigv1.FlowSlot) bool {
		caps := m.GetCapabilities()
		var family bool
		switch sl.GetCapability() {
		case string(modelconfig.CapabilityVision):
			family = m.GetCapability() == string(modelconfig.CapabilityChat) && caps.GetVisionInput()
		case string(modelconfig.CapabilityTranscribe):
			family = m.GetCapability() == string(modelconfig.CapabilityChat) && caps.GetAudioInput()
		default:
			family = m.GetCapability() == sl.GetCapability()
		}
		vendors := sl.GetVendors()
		return family && (len(vendors) == 0 || slices.Contains(vendors, m.GetVendor()))
	}
	for _, f := range fr.GetFlows() {
		for _, sl := range f.GetSlots() {
			slot, _ := modelconfig.LookupSlot(f.GetKey(), sl.GetKey())
			var offered []string
			for _, m := range mr.GetModels() {
				if !fits(m, sl) {
					continue
				}
				offered = append(offered, m.GetId())
				if u := unmetForSlot(slot, m.GetId()); len(u) != 0 {
					t.Errorf("%s/%s: picker offers %s but the server rejects it: %v", f.GetKey(), sl.GetKey(), m.GetId(), u)
				}
			}
			if len(offered) == 0 {
				t.Errorf("%s/%s: picker is empty", f.GetKey(), sl.GetKey())
			}
		}
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

	// Vision slot through the real prober: static pass, no live probe.
	resp, _ = newModelConfigAdminService(nil, modelprobe.New(nil)).TestSlotModel(ctx, req(modelconfig.FlowVision, modelconfig.SlotExtract, "gemini-2.5-pro"))
	if !resp.GetPassed() || !strings.Contains(resp.GetDetail(), "no live probe") {
		t.Fatalf("vision slot should static-pass without a live probe: %+v", resp)
	}
	// Claude into a vision slot fails statically.
	resp, _ = newModelConfigAdminService(nil, nil).TestSlotModel(ctx, req(modelconfig.FlowVision, modelconfig.SlotExtract, "claude-sonnet-4-5-20250929"))
	if resp.GetPassed() || len(resp.GetUnmetRequirements()) == 0 {
		t.Fatalf("Claude on a vision slot should fail statically: %+v", resp)
	}

	// Nil prober → static-only pass.
	resp, _ = newModelConfigAdminService(nil, nil).TestSlotModel(ctx, req(modelconfig.FlowContentPlan, modelconfig.SlotMain, "claude-sonnet-4-5-20250929"))
	if !resp.GetPassed() {
		t.Fatalf("nil prober should static-pass: %+v", resp)
	}
}
