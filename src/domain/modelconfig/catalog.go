// Package modelconfig is the code-owned catalog of configurable genkit flows
// and their model "slots". A slot is one model-selection site: a
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

import (
	"fmt"
	"slices"
)

// Capability is the coarse model family a slot needs. Chat covers every
// text-generation flow (Generate / GenerateStream / GenerateData); embed covers
// the embedding pipeline. Vision and transcribe are chat models with image or
// audio input, run by image-service / audio-service with the model id ogen
// passes on each request.
type Capability string

const (
	CapabilityChat       Capability = "chat"
	CapabilityEmbed      Capability = "embed"
	CapabilityVision     Capability = "vision"
	CapabilityTranscribe Capability = "transcribe"
)

// Vendor slugs a slot can be restricted to. The vendor registry names its
// descriptors from these (llm.VendorAnthropic / llm.VendorGemini), so the two
// can't drift.
const (
	VendorAnthropic = "anthropic"
	VendorGemini    = "gemini"
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
	FlowVision            = "vision"
	FlowTranscribe        = "transcribe"
)

// Slot keys — stable identifiers persisted in flow_model_config.slot_key.
const (
	SlotMain         = "main"
	SlotPlanner      = "planner"
	SlotWriter       = "writer"
	SlotOrchestrator = "orchestrator"
	SlotClassify     = "classify"
	SlotExtract      = "extract"
	SlotEscalate     = "escalate"
	SlotAltText      = "alt_text"
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
	VisionInput      bool // accepts image input (image-service vision calls)
	AudioInput       bool // accepts audio input (audio-service transcription)
}

// Supports reports whether a model can serve a slot of the given capability.
// Vision and transcribe are chat models with the matching input modality; the
// other capabilities must match exactly.
func (c ModelCapabilities) Supports(want Capability) bool {
	switch want {
	case CapabilityVision:
		return c.Capability == CapabilityChat && c.VisionInput
	case CapabilityTranscribe:
		return c.Capability == CapabilityChat && c.AudioInput
	default:
		return c.Capability == want
	}
}

// Slot is one model-selection site within a flow.
type Slot struct {
	Key         string
	Description string
	Capability  Capability
	GlobalOnly  bool // true → no per-tier override (the embed slot, §D5)
	// Vendors restricts which vendors' models may fill the slot; empty = any.
	// In-process chat slots build Anthropic call configs, and image-service /
	// audio-service only call Gemini.
	Vendors  []string
	Requires Requirements
}

// AllowsVendor reports whether a model from vendor may fill the slot.
func (s Slot) AllowsVendor(vendor string) bool {
	return len(s.Vendors) == 0 || slices.Contains(s.Vendors, vendor)
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
		{Key: SlotMain, Description: "Generates the plan's posts.", Capability: CapabilityChat, Vendors: anthropicOnly,
			Requires: Requirements{NeedsStreaming: true, NeedsStructuredOutput: true, MinMaxOutputTokens: 64000}},
	}},
	{Key: FlowDraftPost, Description: "Targeted content drafting from research.", Slots: []Slot{
		{Key: SlotMain, Description: "Writes the draft.", Capability: CapabilityChat, Vendors: anthropicOnly},
	}},
	{Key: FlowEnrichBrief, Description: "Campaign brief enrichment.", Slots: []Slot{
		{Key: SlotMain, Description: "Expands the brief.", Capability: CapabilityChat, Vendors: anthropicOnly},
	}},
	{Key: FlowConsistency, Description: "Brief/posts consistency review.", Slots: []Slot{
		{Key: SlotMain, Description: "Scores consistency.", Capability: CapabilityChat, Vendors: anthropicOnly,
			Requires: Requirements{NeedsStructuredOutput: true}},
	}},
	{Key: FlowPostQuality, Description: "Per-dimension post quality assessment.", Slots: []Slot{
		{Key: SlotMain, Description: "Scores quality dimensions.", Capability: CapabilityChat, Vendors: anthropicOnly,
			Requires: Requirements{NeedsStructuredOutput: true}},
	}},
	{Key: FlowPostAssistant, Description: "Interactive post editing assistant (hybrid).", Slots: []Slot{
		{Key: SlotPlanner, Description: "Cheap orchestration/routing loop.", Capability: CapabilityChat, Vendors: anthropicOnly,
			Requires: Requirements{NeedsTools: true}},
		{Key: SlotWriter, Description: "Capable copywriter behind the editPost tool.", Capability: CapabilityChat, Vendors: anthropicOnly},
	}},
	{Key: FlowCampaignAssistant, Description: "Campaign orchestration assistant.", Slots: []Slot{
		{Key: SlotOrchestrator, Description: "Cheap orchestration/routing loop (delegates to sub-flows).", Capability: CapabilityChat, Vendors: anthropicOnly,
			Requires: Requirements{NeedsTools: true}},
	}},
	{Key: FlowEmbed, Description: "Embedding pipeline (assets, PDFs, queries).", Slots: []Slot{
		{Key: SlotMain, Description: "Embeds text into the shared vector space.", Capability: CapabilityEmbed, GlobalOnly: true,
			Requires: Requirements{EmbedDims: 3072}},
	}},
	{Key: FlowVision, Description: "Image understanding in image-service (asset ingestion, alt text).", Slots: []Slot{
		{Key: SlotClassify, Description: "Cheap first pass that classifies the image.", Capability: CapabilityVision, Vendors: geminiOnly},
		{Key: SlotExtract, Description: "High-resolution extraction of text, tables and description.", Capability: CapabilityVision, Vendors: geminiOnly},
		{Key: SlotEscalate, Description: "One-shot re-extraction when classification confidence is low.", Capability: CapabilityVision, Vendors: geminiOnly},
		{Key: SlotAltText, Description: "Alt text for uploaded images and post attachments.", Capability: CapabilityVision, Vendors: geminiOnly},
	}},
	{Key: FlowTranscribe, Description: "Audio transcription in audio-service.", Slots: []Slot{
		{Key: SlotMain, Description: "Transcribes each audio segment.", Capability: CapabilityTranscribe, Vendors: geminiOnly},
	}},
}

var (
	anthropicOnly = []string{VendorAnthropic}
	geminiOnly    = []string{VendorGemini}
)

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
// Requirements. The capability family is checked separately by the caller
// (ModelCapabilities.Supports against Slot.Capability).
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
