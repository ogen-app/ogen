// Package llm registers Ogen's foundation-model vendors — Anthropic (Claude
// generation) and Gemini (embeddings) — into the vendor registry, and exposes
// a Provider that resolves model references by logical role. Flows depend on a
// role ("generation"/"quality") and this package, not on a hardcoded
// "anthropic/" prefix or the Anthropic SDK config type (CON-86 D8/FR12).
//
// Importing this package registers the vendors via init(), mirroring the River
// job self-registration idiom — adding a model vendor is one file.
package llm

import (
	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/infra/vendors"
)

// Vendor name slugs — the `vendor` dimension on usage events, and (for
// Anthropic) the genkit "provider/model" prefix.
const (
	VendorAnthropic = "anthropic"
	VendorGemini    = "gemini"
)

// priceVersion tags the rate set below so historical cost is never recomputed
// from edited prices (CON-86 FR3). Bump on any rate change. Rates verified
// 2026-09-11 against platform.claude.com (Sonnet 4.5 $3/$15, Haiku 4.5 $1/$5;
// cache-read 0.1×, cache-write-5m 1.25×) and ai.google.dev (Gemini Embedding
// $0.15/1M input; Gemini 2.5 Flash: audio input $1.00/1M, output $2.50/1M —
// transcription input is audio, so KindInput carries the audio rate, CON-282).
const priceVersion = "2026-09-11"

func init() {
	vendors.Register(vendors.Descriptor{
		Name:      VendorAnthropic,
		Family:    vendors.FamilyModel,
		SecretKey: "anthropic_api_key", // must match secrets.NameAnthropicAPIKey
		Metered:   true,
		Meter:     genkitGenerateMeter{},
		Prices: vendors.PriceTable{
			Version: priceVersion,
			Models: map[string]vendors.Rates{
				"claude-sonnet-4-5-20250929": {
					vendors.KindInput:         3_000_000,
					vendors.KindOutput:        15_000_000,
					vendors.KindCacheRead:     300_000,
					vendors.KindCacheCreation: 3_750_000,
				},
				"claude-haiku-4-5-20251001": {
					vendors.KindInput:         1_000_000,
					vendors.KindOutput:        5_000_000,
					vendors.KindCacheRead:     100_000,
					vendors.KindCacheCreation: 1_250_000,
				},
			},
		},
	})

	vendors.Register(vendors.Descriptor{
		Name:      VendorGemini,
		Family:    vendors.FamilyModel,
		SecretKey: "gemini_api_key", // must match secrets.NameGeminiAPIKey (CON-104)
		Metered:   true,
		Meter:     geminiMeter{},
		Prices: vendors.PriceTable{
			Version: priceVersion,
			Models: map[string]vendors.Rates{
				"gemini-embedding-2": {vendors.KindEmbedInput: 150_000},
				// CON-282: audio transcription reuses the gemini vendor with a new
				// model id + input/output token rates (no new vendor). The model id
				// is config (TRANSCRIBE_MODEL); keep this key in sync so runs are
				// priced rather than counted as unknown-model (cost stays 0). Input
				// is billed at the AUDIO rate ($1.00/1M) — a transcription request's
				// prompt tokens are almost entirely the segment audio.
				"gemini-2.5-flash": {
					vendors.KindInput:  1_000_000,
					vendors.KindOutput: 2_500_000,
				},
				// CON-281: image-service vision runs on the same gemini vendor. The
				// service returns per-call token usage; ogen prices it here. classify
				// uses 2.5-flash (already priced above; its input rate is the audio
				// rate, a small over-estimate for image input — acceptable v1, cost is
				// a snapshot). extract/escalate use 2.5-pro, priced at its own
				// input/output rates ($1.25/1M in, $10/1M out; ai.google.dev). Keep
				// these keys in sync with VISION_*_MODEL so runs are priced, not
				// counted as unknown-model (cost 0).
				"gemini-2.5-pro": {
					vendors.KindInput:  1_250_000,
					vendors.KindOutput: 10_000_000,
				},
			},
		},
	})
}

// genkitGenerateMeter extracts token usage from a genkit *ai.ModelResponse.
// Both Anthropic generation flows use it — the mapping is identical across
// genkit model vendors; only the price table differs per vendor.
//
// genkit's normalized usage exposes cache_read (as CachedContentTokens) but
// NOT cache_creation (the Anthropic plugin drops it), so cache_creation is
// left unset (0) and understates cached-write cost until a raw-response path
// lands (CON-86 FR2, §10).
type genkitGenerateMeter struct{}

func (genkitGenerateMeter) Extract(resp any) (string, vendors.Usage, bool) {
	mr, ok := resp.(*ai.ModelResponse)
	if !ok || mr == nil || mr.Usage == nil {
		return "", nil, false
	}
	u := vendors.Usage{}
	addToken(u, vendors.KindInput, mr.Usage.InputTokens)
	addToken(u, vendors.KindOutput, mr.Usage.OutputTokens)
	addToken(u, vendors.KindCacheRead, mr.Usage.CachedContentTokens)
	addToken(u, vendors.KindReasoning, mr.Usage.ThoughtsTokens)
	if len(u) == 0 {
		return "", nil, false
	}
	return "generate", u, true
}

// EmbedUsage is what an embedding call site hands to the Gemini meter: the
// embed response carries no token count, so the caller computes it (from
// assets_chunks.token_count) and passes it here (CON-86 FR2).
type EmbedUsage struct {
	Tokens int64
}

// TranscribeUsage is what an audio-transcription call site hands to the Gemini
// meter (CON-282): audio-service returns Gemini's own input/output token counts
// per segment, which ogen sums and prices via the gemini vendor's input/output
// rates — the same vendor the embedder uses, no new vendor.
type TranscribeUsage struct {
	InputTokens  int64
	OutputTokens int64
}

// VisionUsage is what an image-vision call site hands to the Gemini meter
// (CON-281): image-service returns Gemini's own input/output token counts per
// vision call, tagged with the pipeline Step (vision_classify / vision_extract /
// alt_text / describe) which becomes the usage event's Operation. Priced via the
// gemini vendor's input/output rates — the same vendor the embedder + transcriber
// use, no new vendor.
type VisionUsage struct {
	Step         string
	InputTokens  int64
	OutputTokens int64
}

// geminiMeter is the single meter registered for the gemini vendor. It handles
// every operation the vendor bills: embeddings (EmbedUsage), audio transcription
// (TranscribeUsage), and image vision (VisionUsage). RecordResp dispatches on the
// response type, so a call site only chooses which usage struct to hand in.
type geminiMeter struct{}

func (geminiMeter) Extract(resp any) (string, vendors.Usage, bool) {
	switch v := resp.(type) {
	case EmbedUsage:
		if v.Tokens <= 0 {
			return "", nil, false
		}
		return "embed", vendors.Usage{vendors.KindEmbedInput: v.Tokens}, true
	case TranscribeUsage:
		u := vendors.Usage{}
		if v.InputTokens > 0 {
			u[vendors.KindInput] = v.InputTokens
		}
		if v.OutputTokens > 0 {
			u[vendors.KindOutput] = v.OutputTokens
		}
		if len(u) == 0 {
			return "", nil, false
		}
		return "transcribe", u, true
	case VisionUsage:
		u := vendors.Usage{}
		if v.InputTokens > 0 {
			u[vendors.KindInput] = v.InputTokens
		}
		if v.OutputTokens > 0 {
			u[vendors.KindOutput] = v.OutputTokens
		}
		if len(u) == 0 {
			return "", nil, false
		}
		step := v.Step
		if step == "" {
			step = "vision"
		}
		return step, u, true
	default:
		return "", nil, false
	}
}

func addToken(u vendors.Usage, k vendors.Kind, n int) {
	if n != 0 {
		u[k] = int64(n)
	}
}
