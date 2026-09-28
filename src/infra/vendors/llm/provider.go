package llm

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
)

// Provider carries the vendor-specific call plumbing flows need without
// importing the Anthropic SDK. The model itself is chosen per call by the
// modelconfig resolver. Constructed once and shared.
type Provider struct{}

// NewProvider builds the shared Provider.
func NewProvider() *Provider { return &Provider{} }

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
