// Package modelprobe runs the CON-308 §8a live "golden probe": a minimal, real
// model call used by ModelConfigAdminService.TestSlotModel to verify a candidate
// model actually works — catching what the static requirements match cannot
// (missing/rotated API key, plugin not registered, unknown model id, region
// gating, wrong embedding dimensionality). Chat slots run a 1-token Anthropic
// generation; embed slots run a real Gemini embedding and check the returned
// dimensionality. Any other capability reports ErrProbeUnsupported.
package modelprobe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	anthropicplugin "github.com/firebase/genkit/go/plugins/anthropic"
	"github.com/firebase/genkit/go/plugins/googlegenai"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// ErrProbeUnsupported signals that no live probe applies to this slot's
// capability. The caller keeps the static-only verdict rather than reporting a
// failure. (No current slot hits this — chat and embed are both probed — but
// CON-310's vision/transcription slots will until they grow a probe.)
var ErrProbeUnsupported = errors.New("modelprobe: no live probe for this slot")

// probeTimeout bounds a single probe so a hung provider can't stall the operator.
const probeTimeout = 15 * time.Second

// Runner builds a throwaway genkit instance per probe from the current API key
// in the secrets store (mirroring the flow runtime / embedder rebuild), so a key
// set or rotated via the secrets API is picked up with no restart.
type Runner struct {
	store secrets.Store
}

// New returns a Runner backed by the secrets store.
func New(store secrets.Store) *Runner { return &Runner{store: store} }

// Probe runs the smallest real call that proves the model is reachable and
// answering, returning a short output sample and the call latency. It assumes
// the static requirements already passed (the caller runs that first).
func (r *Runner) Probe(ctx context.Context, flowKey, slotKey, modelID string) (string, int64, error) {
	slot, ok := modelconfig.LookupSlot(flowKey, slotKey)
	if !ok {
		return "", 0, fmt.Errorf("unknown flow/slot %q/%q", flowKey, slotKey)
	}
	vendor, ok := vendors.VendorOf(modelID)
	if !ok {
		return "", 0, fmt.Errorf("unknown model %q", modelID)
	}
	if r.store == nil {
		return "", 0, errors.New("secrets store unavailable")
	}
	switch slot.Capability {
	case modelconfig.CapabilityChat:
		return r.probeChat(ctx, vendor, modelID)
	case modelconfig.CapabilityEmbed:
		return r.probeEmbed(ctx, vendor, modelID, slot.Requires.EmbedDims)
	default:
		return "", 0, ErrProbeUnsupported
	}
}

// probeChat runs a 1-token generation. v1 chat is Anthropic-only.
func (r *Runner) probeChat(ctx context.Context, vendor, modelID string) (string, int64, error) {
	if vendor != llm.VendorAnthropic {
		return "", 0, fmt.Errorf("chat probe supports Anthropic models only (got vendor %q)", vendor)
	}
	key, err := r.store.Get(ctx, secrets.NameAnthropicAPIKey)
	if err != nil {
		return "", 0, fmt.Errorf("vendor not live: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	g := genkit.Init(pctx, genkit.WithPlugins(&anthropicplugin.Anthropic{APIKey: key}))

	start := time.Now()
	resp, err := genkit.Generate(pctx, g,
		ai.WithModelName(vendor+"/"+modelID),
		ai.WithPrompt("Reply with the single word: ok"),
		ai.WithConfig(anthropic.MessageNewParams{MaxTokens: 16}),
	)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, err
	}
	sample := strings.TrimSpace(resp.Text())
	if sample == "" {
		return "", latency, errors.New("model returned an empty response")
	}
	return sample, latency, nil
}

// probeEmbed embeds a short string and verifies the returned vector matches the
// slot's required dimensionality — catching a wrong/incompatible embed model
// that would otherwise fail only when inserting into the assets_chunks
// halfvec(N) column. v1 embed is Gemini-only.
func (r *Runner) probeEmbed(ctx context.Context, vendor, modelID string, wantDims int) (string, int64, error) {
	if vendor != llm.VendorGemini {
		return "", 0, fmt.Errorf("embed probe supports Gemini models only (got vendor %q)", vendor)
	}
	key, err := r.store.Get(ctx, secrets.NameGeminiAPIKey)
	if err != nil {
		return "", 0, fmt.Errorf("vendor not live: %w", err)
	}
	dims := wantDims
	if dims <= 0 {
		dims = int(embedopts.Dimensions)
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	plugin := &googlegenai.GoogleAI{APIKey: key}
	g := genkit.Init(pctx, genkit.WithPlugins(plugin))
	// A fresh instance per probe, so re-defining the same embedder name across
	// probes never conflicts (mirrors the embedder rebuild in infra/embedding).
	embedder, err := plugin.DefineEmbedder(g, modelID, &ai.EmbedderOptions{Dimensions: dims})
	if err != nil {
		return "", 0, fmt.Errorf("init embedder: %w", err)
	}

	start := time.Now()
	resp, err := embedder.Embed(pctx, &ai.EmbedRequest{
		Input:   []*ai.Document{ai.DocumentFromText("probe", nil)},
		Options: embedopts.Query(),
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, err
	}
	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0].Embedding) == 0 {
		return "", latency, errors.New("embedder returned no vector")
	}
	got := len(resp.Embeddings[0].Embedding)
	if wantDims > 0 && got != wantDims {
		return "", latency, fmt.Errorf("embedding dimensionality %d != required %d (would break the halfvec(%d) column)", got, wantDims, wantDims)
	}
	return fmt.Sprintf("%d-dim embedding", got), latency, nil
}
