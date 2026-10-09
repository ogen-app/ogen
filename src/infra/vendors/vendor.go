// Package vendor is the single registry of external service vendors Ogen
// calls — foundation-model providers (Anthropic, Gemini) and content
// publishers (Zernio) — together with the vendor-neutral metering
// primitives the usage-accounting layer is built on.
//
// Adding a vendor means dropping one file that calls Register() from an
// init(); there is no central list to edit. This mirrors the River job
// self-registration idiom in src/jobs/queues/river.go.
//
// Vendor families share only two things: this registration pattern
// (Layer 0) and the optional metering cross-cut — the Meter interface plus
// Cost (Layer 2). Each family keeps its own behavioural contract — model
// providers generate/embed, publishers discover/publish — so there is
// deliberately NO single "Vendor" behavioural interface.
package vendors

// Family groups vendors by their behavioural contract.
type Family string

const (
	FamilyModel     Family = "model"
	FamilyPublisher Family = "publisher"
	// FamilyIngest groups content-ingestion vendors — web scrapers, file
	// extractors — metered by count of ingested items. Kept separate
	// from FamilyPublisher so scrape volume rolls up on its own axis.
	FamilyIngest Family = "ingest"
)

// Kind is an open category of billable unit. Model vendors report token
// kinds; publishers report action kinds. The set is intentionally open: a
// new kind never touches the cost math, because Cost ranges over whatever
// kinds appear in the Usage map.
type Kind string

const (
	// Model token kinds.
	KindInput         Kind = "input"
	KindOutput        Kind = "output"
	KindCacheRead     Kind = "cache_read"     // Anthropic prompt-cache read
	KindCacheCreation Kind = "cache_creation" // Anthropic prompt-cache write
	KindReasoning     Kind = "reasoning"      // reasoning / thinking tokens
	KindEmbedInput    Kind = "embed_input"    // embedding input tokens

	// Publisher action kinds.
	KindPost    Kind = "post"     // one published or scheduled post
	KindAPICall Kind = "api_call" // one billable API round-trip
	KindAccount Kind = "account"  // one connected social account

	// Ingest action kinds.
	KindURLScrape Kind = "url_scrape" // one Firecrawl URL scrape
)

// Usage is the vendor-neutral unit breakdown for a single call. A model
// generate is e.g. {input: 1200, output: 800, cache_read: 400}; a Zernio
// publish is {post: 1}. A nil or empty map means "nothing to charge".
type Usage map[Kind]int64

// Rates maps a Kind to its price in USD-micros per 1,000,000 units. The
// per-million normalisation keeps token rates whole (e.g. $3/1M tokens =>
// 3_000_000) while still expressing per-action prices exactly (e.g. $0.10
// per post => 100_000_000_000 per 1M posts). A kind absent from the map is
// priced at 0.
type Rates map[Kind]int64

// PriceTable is a versioned set of per-model (or per-sku) rate tables.
// Version snapshots which rate set produced a cost so historical cost is
// never recomputed from edited prices. The zero value (empty
// Models) prices everything at 0 — the count-only default for vendors whose
// per-unit cost we do not yet track.
type PriceTable struct {
	Version string           // e.g. "anthropic-2026-06"
	Models  map[string]Rates // model id / sku -> per-kind rates
	// LongPrompt holds, per model id, the rate set that replaces Models[id]
	// for a request whose prompt exceeds the tier's threshold. Optional: a
	// model absent here is priced at Models[id] regardless of prompt length.
	LongPrompt map[string]PromptTier
}

// PromptTier prices a whole request at Rates — output included — once its
// prompt (input + cache-read + cache-write tokens) exceeds Threshold tokens.
type PromptTier struct {
	Threshold int64
	Rates     Rates
}

// RatesFor returns the rates that price usage u on model. ok is false when the
// model has no entry in Models.
func (p PriceTable) RatesFor(model string, u Usage) (Rates, bool) {
	rates, ok := p.Models[model]
	if !ok {
		return nil, false
	}
	if tier, ok := p.LongPrompt[model]; ok && promptTokens(u) > tier.Threshold {
		return tier.Rates, true
	}
	return rates, true
}

// promptTokens is the prompt length a long-prompt tier is measured against:
// every input token the request sent, whether fresh, read from cache or
// written to it.
func promptTokens(u Usage) int64 {
	return u[KindInput] + u[KindCacheRead] + u[KindCacheCreation]
}

const perMillion = 1_000_000

// Cost computes the snapshot cost in USD-micros for a usage breakdown under
// a single rate table:
//
//	cost_micros = Σ over kinds: units[kind] * rates[kind] / 1_000_000
//
// using integer (truncating) math — analytics-grade, not billing-grade.
// A kind present in usage but missing from rates contributes 0;
// that gap is the caller's signal to widen the rate table.
func Cost(u Usage, r Rates) int64 {
	var micros int64
	for kind, units := range u {
		if units == 0 {
			continue
		}
		rate, ok := r[kind]
		if !ok {
			continue
		}
		micros += units * rate / perMillion
	}
	return micros
}
