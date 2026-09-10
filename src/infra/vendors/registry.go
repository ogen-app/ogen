package vendors

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Descriptor is the registration record a vendor declares from its init().
// It is the single source of truth for vendor identity: business logic
// looks vendors up here rather than hardcoding provider names (the
// "anthropic/" prefix, plugin struct, and secret name were the three ad-hoc
// places before CON-86).
type Descriptor struct {
	// Name is the stable slug ("anthropic", "gemini", "zernio"). It is the
	// `vendor` dimension on every usage event and the registry key.
	Name string
	// Family is the behavioural contract this vendor belongs to.
	Family Family
	// SecretKey is the secrets.Name… under which this vendor's API key is
	// stored, for hot-reload subscription. Empty when the key comes from
	// config/env (e.g. Gemini today) or the vendor needs no key.
	SecretKey string
	// Metered reports whether this vendor emits usage events.
	Metered bool
	// Prices is the versioned per-model rate table used to snapshot cost at
	// write time. An empty Models map is "count-only": every event costs 0
	// (intentional, not an unknown-model gap) — used by publishers we count
	// but don't yet price.
	Prices PriceTable
	// Meter normalises a provider response into (operation, Usage) for
	// RecordResp. Nil for vendors that build MeterEvents directly and call
	// Record (publishers) — only the RecordResp convenience path uses it.
	Meter Meter
}

// Meter is the optional metering cross-cut a vendor implements to be
// accounted. It is the only behaviour the registry knows about a vendor;
// everything family-specific lives behind the family's own contract.
type Meter interface {
	// Extract normalises one completed call's result into an operation label
	// and a unit breakdown. resp is family-specific — a *ai.ModelResponse, an
	// embed-usage struct, etc. — and implementations type-assert it. The model
	// id and feature are NOT read from resp (they aren't reliably present in a
	// genkit response); the caller supplies them. ok=false means "nothing to
	// record" (unrecognised or empty response); the recorder then skips.
	Extract(resp any) (operation string, u Usage, ok bool)
}

// MeterEvent is the vendor-neutral payload a Meter produces for one call.
// The recorder stamps tenant, vendor, family, feature, cost and timestamp
// around it; the Meter supplies only what it can read from the response.
type MeterEvent struct {
	Model     string // model id or publisher sku/tier; keys into PriceTable.Models
	Operation string // generate | embed | publish | analytics_refresh | account_connect | …
	Usage     Usage

	// Publisher dimensions. Zero/empty for model events.
	Platform        string
	SocialAccountID string
}

var (
	mu       sync.RWMutex
	registry = map[string]Descriptor{}
)

// Register records a vendor descriptor. Called from a vendor file's init().
// It panics on a duplicate name or an obviously invalid descriptor: these
// are programming errors that must fail at process start, exactly like the
// secrets allowlist (a deliberate, reviewed code change — not config).
func Register(d Descriptor) {
	switch {
	case d.Name == "":
		panic("vendor: Register with empty Name")
	case d.Family == "":
		panic(fmt.Sprintf("vendor: Register %q with empty Family", d.Name))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[d.Name]; dup {
		panic(fmt.Sprintf("vendor: duplicate Register for %q", d.Name))
	}
	registry[d.Name] = d
}

// Get returns the descriptor registered under name.
func Get(name string) (Descriptor, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

// All returns every registered descriptor, ordered by name for determinism.
func All() []Descriptor {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Descriptor) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// ByFamily returns the registered descriptors in a family, ordered by name.
func ByFamily(f Family) []Descriptor {
	all := All()
	out := all[:0]
	for _, d := range all {
		if d.Family == f {
			out = append(out, d)
		}
	}
	return out
}

// CostOf computes the snapshot cost for a usage breakdown under the named
// vendor's current price table. ok is false when the vendor is unknown or
// the model/sku has no rate entry; callers should still record the event
// with cost 0 and bump an "unknown model"/"unknown vendor" counter
// (CON-86 FR3, §9). The returned version is "" only when the vendor itself
// is unknown.
func CostOf(vendorName, model string, u Usage) (micros int64, version string, ok bool) {
	d, found := Get(vendorName)
	if !found {
		return 0, "", false
	}
	rates, found := d.Prices.Models[model]
	if !found {
		return 0, d.Prices.Version, false
	}
	return Cost(u, rates), d.Prices.Version, true
}

// MergePrices merges per-model rate overrides into a registered vendor's price
// table and re-tags the table with version (boot-time, from USAGE_MODEL_PRICES).
// Models present in the override replace the in-code defaults; other models are
// kept. It returns false when the vendor isn't registered. Copy-on-write: a
// fresh Models map is built so already-handed-out descriptors aren't mutated.
func MergePrices(vendorName, version string, models map[string]Rates) bool {
	mu.Lock()
	defer mu.Unlock()
	d, ok := registry[vendorName]
	if !ok {
		return false
	}
	merged := make(map[string]Rates, len(d.Prices.Models)+len(models))
	maps.Copy(merged, d.Prices.Models)
	maps.Copy(merged, models)
	d.Prices = PriceTable{Version: version, Models: merged}
	registry[vendorName] = d
	return true
}
