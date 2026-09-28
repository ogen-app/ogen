package llm

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
)

// Role selects which configured model a flow wants. content_plan,
// post_assistant, and enrich_brief use generation; post_quality uses quality;
// the campaign_assistant orchestration/routing loop uses planning —
// a cheap fast model, while the prose-writing sub-flows it invokes as tools
// stay on generation.
type Role string

const (
	RoleGeneration Role = "generation"
	RoleQuality    Role = "quality"
	RolePlanning   Role = "planning"
)

// Provider resolves foundation-model references by role so flows don't
// hardcode the "anthropic/" prefix or carry the configured model ids
// themselves. Constructed once from config and shared.
type Provider struct {
	generationModel string
	qualityModel    string
	planningModel   string
}

// NewProvider builds a Provider from the configured model ids
// (cfg.ModelID, cfg.QualityModelID, cfg.PlanningModelID).
func NewProvider(generationModel, qualityModel, planningModel string) *Provider {
	return &Provider{
		generationModel: generationModel,
		qualityModel:    qualityModel,
		planningModel:   planningModel,
	}
}

// Model returns the bare model id for a role — the value recorded as the
// `model` dimension and looked up in the price map.
func (p *Provider) Model(role Role) string {
	switch role {
	case RoleQuality:
		return p.qualityModel
	case RolePlanning:
		return p.planningModel
	default:
		return p.generationModel
	}
}

// Ref returns the genkit "provider/model" reference for ai.WithModelName,
// e.g. "anthropic/claude-sonnet-4-5-20250929".
func (p *Provider) Ref(role Role) string {
	return VendorAnthropic + "/" + p.Model(role)
}

// Vendor returns the vendor slug backing these roles — the `vendor` dimension
// recorded for generation/quality calls. Single-vendor today.
func (p *Provider) Vendor() string {
	return VendorAnthropic
}

// CallConfig builds the genkit config option carrying the max-tokens setting,
// so flows pass it without importing the Anthropic SDK. The
// returned ConfigOption satisfies ai.GenerateOption and is accepted by
// genkit.Generate, GenerateStream, and GenerateData alike. WithConfig rejects
// being set twice, so a flow must pass exactly one CallConfig per call.
func (p *Provider) CallConfig(maxTokens int64) ai.GenerateOption {
	return ai.WithConfig(anthropic.MessageNewParams{MaxTokens: maxTokens})
}
