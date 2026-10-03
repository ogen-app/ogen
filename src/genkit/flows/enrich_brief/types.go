package enrich_brief

import (
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// EnrichBriefRequest is the input to the enrichBrief flow.
type EnrichBriefRequest struct {
	CampaignID string `json:"campaignId"`
	// Instruction is optional freeform steering ("B2B, technical tone").
	// With an existing brief it says what to change; the rest is kept.
	Instruction string `json:"instruction,omitempty"`
}

// EnrichBriefResponse is the generated campaign brief. Field order is the
// streaming order: the model emits keys top-to-bottom, so the four
// user-visible fields appear in the order the UI reads them. Every field
// carries a jsonschema description so the prompt and any future
// schema-driven tooling stay in sync.
type EnrichBriefResponse struct {
	Description    string `json:"description"    jsonschema:"description=2-4 sentence campaign overview"`
	TargetPersona  string `json:"targetPersona"  jsonschema:"description=Who the campaign speaks to"`
	KeyMessages    string `json:"keyMessages"    jsonschema:"description=3-5 key messages, one per line"`
	ToneGuidelines string `json:"toneGuidelines" jsonschema:"description=Voice and style guidance"`
}

// EnrichBriefRepos bundles the repository dependencies for the flow.
// CampaignTypes is a fallback: CampaignRepository.GetByID already hydrates
// the campaign's type (with phases), but the repo lets the flow resolve a
// type directly if a future caller passes an un-hydrated campaign. Brands
// supplies the brand voice and guardrails; nil skips them.
type EnrichBriefRepos struct {
	Campaigns     repository.CampaignRepository
	CampaignTypes repository.CampaignTypeRepository
	Brands        repository.BrandRepository
}

// EnrichBriefFlowConfig holds static settings for the flow.
type EnrichBriefFlowConfig struct {
	// Provider resolves the model reference + call config by role.
	Provider *llm.Provider
	// Recorder captures usage events; nil disables recording.
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	// MaxOutputTokens caps a single model call. 0 falls back to 32768 — a
	// brief is well under that, so truncation should never fire; the cap is
	// just a guard.
	MaxOutputTokens int64
}

// ValidationError is returned when preconditions are not met (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is returned when the model call fails or returns nothing usable
// (HTTP 502).
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE types ─────────────────────────────────────────────────────────────────

// SSEEventKind identifies the SSE event types emitted by the flow.
type SSEEventKind string

const (
	SSEEventStep             SSEEventKind = "step"
	SSEEventDescriptionDelta SSEEventKind = "description_delta"
	SSEEventPersonaDelta     SSEEventKind = "persona_delta"
	SSEEventMessagesDelta    SSEEventKind = "messages_delta"
	SSEEventToneDelta        SSEEventKind = "tone_delta"
	SSEEventComplete         SSEEventKind = "complete"
	SSEEventError            SSEEventKind = "error"
)

// StepEventPayload marks a flow stage as finished.
type StepEventPayload struct {
	Step   string `json:"step"`
	Status string `json:"status"` // always "done"
}

// DeltaEventPayload carries a fragment of a streamed string value. The
// per-field *_delta events are preview-only; clients treat the final
// complete event as the source of truth.
type DeltaEventPayload struct {
	Delta string `json:"delta"`
}

// ErrorEventPayload is emitted when the flow fails mid-stream.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"` // HTTP semantic: 400, 502, 500
}

// OnEventFunc is an optional callback invoked as SSE events are produced.
// A nil OnEventFunc is valid — the flow runs silently.
type OnEventFunc func(name SSEEventKind, data any)
