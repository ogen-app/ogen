// Package modelprobe runs the CON-308 §8a live "golden probe": a minimal, real
// model call used by ModelConfigAdminService.TestSlotModel to verify a candidate
// model actually works — catching what the static requirements match cannot
// (missing/rotated API key, plugin not registered, unknown model id, region
// gating). v1 probes CHAT slots (Anthropic); the embed slot is verified
// statically (dimension match) and reports ErrProbeUnsupported here.
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

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// ErrProbeUnsupported signals that no live probe applies to this slot (v1: any
// non-chat slot, e.g. embed). The caller keeps the static-only verdict rather
// than reporting a failure.
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
	// v1: only chat slots get a live probe. Embed is verified by the static
	// dimension match; a live embed probe is a future refinement.
	if slot.Capability != modelconfig.CapabilityChat {
		return "", 0, ErrProbeUnsupported
	}
	if vendor != llm.VendorAnthropic {
		return "", 0, fmt.Errorf("live probe supports Anthropic chat models only (got vendor %q)", vendor)
	}
	if r.store == nil {
		return "", 0, errors.New("secrets store unavailable")
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
