package llm

import (
	"context"
	"errors"
	"expvar"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// ErrRefused is returned by a call guarded by RefusalGuard when the model's
// safety classifiers declined the request. Its text reaches API clients inside
// the flows' error events, so it is worded for them.
var ErrRefused = errors.New("the model declined this request; rephrase it or switch the model")

// modelRefusals counts refused calls, keyed by flow.
var modelRefusals = expvar.NewMap("ogen_model_refusals")

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
//
// The cap covers thinking as well as the answer: Claude 5.x models think by
// default, and their thinking tokens count toward max_tokens.
func (p *Provider) CallConfig(maxTokens int64) ai.GenerateOption {
	return ai.WithConfig(anthropic.MessageNewParams{MaxTokens: maxTokens})
}

// RefusalGuard returns model middleware that fails a call with ErrRefused when
// the model refused it, instead of handing the flow an empty or partial answer
// as if it had finished. It wraps every round of a tool loop. Pass it in the
// call's single ai.WithMiddleware, alongside any other middleware.
func (p *Provider) RefusalGuard(flow string) ai.ModelMiddleware {
	return func(next core.StreamingFunc[*ai.ModelRequest, *ai.ModelResponse, *ai.ModelResponseChunk]) core.StreamingFunc[*ai.ModelRequest, *ai.ModelResponse, *ai.ModelResponseChunk] {
		return func(ctx context.Context, req *ai.ModelRequest, cb core.StreamCallback[*ai.ModelResponseChunk]) (*ai.ModelResponse, error) {
			resp, err := next(ctx, req, cb)
			if err != nil || !refused(resp) {
				return resp, err
			}
			modelRefusals.Add(flow, 1)
			slog.WarnContext(ctx, "model refused the request", logging.AttrComponent, "llm", "flow", flow)
			return nil, ErrRefused
		}
	}
}

// refused reports whether resp is a refusal. The genkit Anthropic plugin maps
// every stop reason it does not know to FinishReasonUnknown. Of those, only
// "refusal" can reach our flows: "pause_turn" needs server tools, which no flow
// declares.
func refused(resp *ai.ModelResponse) bool {
	return resp != nil && resp.FinishReason == ai.FinishReasonUnknown
}
