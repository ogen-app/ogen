// Package modelconfig is the code-owned catalog of configurable genkit flows
// and their model "slots" (CON-308). A slot is one model-selection site: a
// single-model flow has one slot; an orchestrated flow declares several (e.g.
// post_assistant = planner + writer). The catalog is the source of truth for
// which (flow, slot) pairs exist and what a model must be able to do to fill
// each one — it is what ListFlows returns and what the gRPC write path validates
// against.
//
// It also defines the shared capability vocabulary (Capability / Requirements /
// ModelCapabilities) so the vendor registry (which knows what each model can do)
// and the flow catalog (which knows what each slot needs) can be matched without
// the domain depending on infra: the vendor registry imports THIS package to tag
// its models, never the reverse.
package modelconfig

import "fmt"

// Capability is the coarse model family a slot needs. Chat covers every
// text-generation flow (Generate / GenerateStream / GenerateData); embed covers
// the embedding pipeline. Vision/transcription are added by CON-310.
type Capability string

const (
	CapabilityChat  Capability = "chat"
	CapabilityEmbed Capability = "embed"
)

// Flow keys — stable identifiers persisted in flow_model_config.flow_key and
// imported by each flow package so the mapping isn't a loose string literal.
const (
	FlowContentPlan       = "content_plan"
	FlowDraftPost         = "draft_post"
	FlowEnrichBrief       = "enrich_brief"
	FlowConsistency       = "consistency"
	FlowPostQuality       = "post_quality"
	FlowPostAssistant     = "post_assistant"
	FlowCampaignAssistant = "campaign_assistant"
	FlowEmbed             = "embed"
)

// Slot keys — stable identifiers persisted in flow_model_config.slot_key.
const (
	SlotMain         = "main"
	SlotPlanner      = "planner"
	SlotWriter       = "writer"
	SlotOrchestrator = "orchestrator"
)

// Requirements is the hard compatibility contract a slot places on any model
// assigned to it (CON-308 §8a). A zero value means "any model of the slot's
// Capability will do". Fields are checked by Satisfies against a model's
// declared ModelCapabilities; a mismatch blocks the assignment.
type Requirements struct {
	NeedsTools            bool // multi-turn function-calling loop
	NeedsStructuredOutput bool // GenerateData[T] / schema-constrained output
	NeedsStreaming        bool // GenerateStream
	MinMaxOutputTokens    int  // 0 = don't care
	MinContextWindow      int  // 0 = don't care
	EmbedDims             int  // embed slots only; the model's dims must equal this
}

// ModelCapabilities is what a model can do, declared alongside its price in the
// vendor registry. It is matched against a slot's Requirements.
type ModelCapabilities struct {
	Capability       Capability
	Tools            bool
	StructuredOutput bool
	Streaming        bool
	MaxOutputTokens  int
	ContextWindow    int
	EmbedDims        int
}

// Slot is one model-selection site within a flow.
type Slot struct {
	Key         string
	Description string
	Capability  Capability
	GlobalOnly  bool // true → no per-tier override (the embed slot, §D5)
	Requires    Requirements
}

// Flow is a configurable genkit flow and its model slots.
type Flow struct {
	Key         string
	Description string
	Slots       []Slot
}

// catalog is the canonical list. Order is display order (Harbor renders it as
// the config-matrix rows). Keep in sync with the flow call sites (Phase 4).
var catalog = []Flow{
	{Key: FlowContentPlan, Description: "Batch post generation from a campaign brief.", Slots: []Slot{
		{Key: SlotMain, Description: "Generates the plan's posts.", Capability: CapabilityChat,
			Requires: Requirements{NeedsStreaming: true, NeedsStructuredOutput: true, MinMaxOutputTokens: 64000}},
	}},
	{Key: FlowDraftPost, Description: "Targeted content drafting from research.", Slots: []Slot{
		{Key: SlotMain, Description: "Writes the draft.", Capability: CapabilityChat},
	}},
	{Key: FlowEnrichBrief, Description: "Campaign brief enrichment.", Slots: []Slot{
		{Key: SlotMain, Description: "Expands the brief.", Capability: CapabilityChat},
	}},
	{Key: FlowConsistency, Description: "Brief/posts consistency review.", Slots: []Slot{
		{Key: SlotMain, Description: "Scores consistency.", Capability: CapabilityChat,
			Requires: Requirements{NeedsStructuredOutput: true}},
	}},
	{Key: FlowPostQuality, Description: "Per-dimension post quality assessment.", Slots: []Slot{
		{Key: SlotMain, Description: "Scores quality dimensions.", Capability: CapabilityChat,
			Requires: Requirements{NeedsStructuredOutput: true}},
	}},
	{Key: FlowPostAssistant, Description: "Interactive post editing assistant (hybrid).", Slots: []Slot{
		{Key: SlotPlanner, Description: "Cheap orchestration/routing loop.", Capability: CapabilityChat,
			Requires: Requirements{NeedsTools: true}},
		{Key: SlotWriter, Description: "Capable copywriter behind the editPost tool.", Capability: CapabilityChat},
	}},
	{Key: FlowCampaignAssistant, Description: "Campaign orchestration assistant.", Slots: []Slot{
		{Key: SlotOrchestrator, Description: "Cheap orchestration/routing loop (delegates to sub-flows).", Capability: CapabilityChat,
			Requires: Requirements{NeedsTools: true}},
	}},
	{Key: FlowEmbed, Description: "Embedding pipeline (assets, PDFs, queries).", Slots: []Slot{
		{Key: SlotMain, Description: "Embeds text into the shared vector space.", Capability: CapabilityEmbed, GlobalOnly: true,
			Requires: Requirements{EmbedDims: 3072}},
	}},
}

// Flows returns the catalog (display order).
func Flows() []Flow { return catalog }

// SlotRef names one configurable slot.
type SlotRef struct {
	FlowKey string
	Slot    Slot
}

// AllSlots flattens the catalog to (flow, slot) pairs — used by the resolver
// snapshot, the boot reconcile, and the effective-config read.
func AllSlots() []SlotRef {
	var out []SlotRef
	for _, f := range catalog {
		for _, s := range f.Slots {
			out = append(out, SlotRef{FlowKey: f.Key, Slot: s})
		}
	}
	return out
}

// LookupSlot returns the slot for a (flow, slot) pair, or ok=false if the pair
// isn't in the catalog (an unknown assignment target — rejected on write).
func LookupSlot(flowKey, slotKey string) (Slot, bool) {
	for _, f := range catalog {
		if f.Key != flowKey {
			continue
		}
		for _, s := range f.Slots {
			if s.Key == slotKey {
				return s, true
			}
		}
	}
	return Slot{}, false
}

// Satisfies reports the slot requirements a model's capabilities fail to meet.
// An empty result means the model may be assigned to a slot with this
// Requirements. The capability family (chat vs embed) is checked separately by
// the caller against Slot.Capability.
func Satisfies(caps ModelCapabilities, req Requirements) []string {
	var unmet []string
	if req.NeedsTools && !caps.Tools {
		unmet = append(unmet, "tools")
	}
	if req.NeedsStructuredOutput && !caps.StructuredOutput {
		unmet = append(unmet, "structured_output")
	}
	if req.NeedsStreaming && !caps.Streaming {
		unmet = append(unmet, "streaming")
	}
	if req.MinMaxOutputTokens > 0 && caps.MaxOutputTokens < req.MinMaxOutputTokens {
		unmet = append(unmet, fmt.Sprintf("max_output_tokens>=%d", req.MinMaxOutputTokens))
	}
	if req.MinContextWindow > 0 && caps.ContextWindow < req.MinContextWindow {
		unmet = append(unmet, fmt.Sprintf("context_window>=%d", req.MinContextWindow))
	}
	if req.EmbedDims > 0 && caps.EmbedDims != req.EmbedDims {
		unmet = append(unmet, fmt.Sprintf("embed_dims==%d", req.EmbedDims))
	}
	return unmet
}
