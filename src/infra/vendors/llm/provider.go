package llm

import (
	"context"
	"errors"
	"expvar"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"

	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// ErrRefused is returned by a call made through CallMiddleware when the model's
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
// default, and their thinking tokens count toward max_tokens. It is clamped to
// model's max output, so one flow cap serves every tier whatever Claude family
// it runs (64K on 4.x, 128K on 5.x) instead of failing the call on the smaller.
func (p *Provider) CallConfig(model string, maxTokens int64) ai.GenerateOption {
	return ai.WithConfig(anthropic.MessageNewParams{MaxTokens: clampMaxTokens(model, maxTokens)})
}

// clampMaxTokens caps maxTokens at model's declared max output. An unknown
// model is left as asked.
func clampMaxTokens(model string, maxTokens int64) int64 {
	caps, ok := vendors.CapabilitiesOf(VendorAnthropic, model)
	if !ok || caps.MaxOutputTokens <= 0 {
		return maxTokens
	}
	return min(maxTokens, int64(caps.MaxOutputTokens))
}

// CallMiddleware returns the model middleware every flow call carries. It wraps
// each round of a tool loop and:
//
//   - adds the round's prompt-cache write tokens, which the genkit plugin drops,
//     to resp.Usage.Custom[UsageCacheCreation] so they are metered;
//   - fails a refused round with ErrRefused, instead of handing the flow an
//     empty or partial answer as if it had finished. A refusal still consumed
//     tokens, so its response goes to record first; the flow's own metering
//     never sees it. record may be nil.
//
// Pass it in the call's single ai.WithMiddleware, after any metering middleware
// so that middleware sees the cache writes.
func (p *Provider) CallMiddleware(flow string, record func(context.Context, *ai.ModelResponse), opts ...CallOption) ai.ModelMiddleware {
	var o callOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(next core.StreamingFunc[*ai.ModelRequest, *ai.ModelResponse, *ai.ModelResponseChunk]) core.StreamingFunc[*ai.ModelRequest, *ai.ModelResponse, *ai.ModelResponseChunk] {
		return func(ctx context.Context, req *ai.ModelRequest, cb core.StreamCallback[*ai.ModelResponseChunk]) (*ai.ModelResponse, error) {
			ctx, probe := WithCacheWriteProbe(ctx)
			if o.cachePrompt {
				ctx = WithPromptCache(ctx)
			}
			resp, err := next(ctx, req, cb)
			if err != nil {
				return resp, err
			}
			addCacheWrites(resp, probe.Tokens())
			if !refused(resp) {
				return resp, nil
			}
			if record != nil {
				record(ctx, resp)
			}
			modelRefusals.Add(flow, 1)
			slog.WarnContext(ctx, "model refused the request", logging.AttrComponent, "llm", "flow", flow)
			return nil, ErrRefused
		}
	}
}

// CallOption configures CallMiddleware.
type CallOption func(*callOptions)

type callOptions struct {
	cachePrompt bool
}

// CachePrompt asks for the call's prompt to be cached: the system prompt with
// the tools, and the conversation so far. It pays off for a tool loop, whose
// rounds and turns resend the same prefix, and costs a cache-write premium on
// a one-shot call, so only loops ask for it. It applies to whatever model the
// slot resolves to; it is scoped to the model request, so sub-flows a loop
// runs as tools are not cached by it.
func CachePrompt() CallOption {
	return func(o *callOptions) { o.cachePrompt = true }
}

// addCacheWrites stores a call's cache-write tokens on resp's usage.
func addCacheWrites(resp *ai.ModelResponse, tokens int64) {
	if resp == nil || tokens <= 0 {
		return
	}
	if resp.Usage == nil {
		resp.Usage = &ai.GenerationUsage{}
	}
	if resp.Usage.Custom == nil {
		resp.Usage.Custom = map[string]float64{}
	}
	resp.Usage.Custom[UsageCacheCreation] = float64(tokens)
}

// refused reports whether resp is a refusal. The genkit Anthropic plugin maps
// every stop reason it does not know to FinishReasonUnknown. Of those, only
// "refusal" can reach our flows: "pause_turn" needs server tools, which no flow
// declares.
func refused(resp *ai.ModelResponse) bool {
	return resp != nil && resp.FinishReason == ai.FinishReasonUnknown
}
