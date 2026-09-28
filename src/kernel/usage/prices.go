package usage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ogen-app/ogen/src/infra/vendors"
)

// priceOverride is the USAGE_MODEL_PRICES JSON shape: vendor → model/sku →
// kind → USD-micros per 1,000,000 units. Example:
//
//	{"anthropic":{"claude-sonnet-4-5-20250929":{"input":3000000,"output":15000000}},
//	 "zernio":{"instagram":{"post":100000000000}}}
type priceOverride map[string]map[string]map[string]int64

// overrideVersion tags any price table the env override touched, so cost
// snapshots produced from overridden rates are distinguishable.
const overrideVersion = "env-override"

// ApplyModelPrices parses the USAGE_MODEL_PRICES JSON and merges the rate
// overrides into the registered vendors. Empty/blank is a
// no-op. A malformed payload or an unknown vendor returns an error so the
// misconfiguration fails fast at boot — mirroring QualityWeightProfiles. Must
// run before any usage is recorded (boot, single goroutine).
func ApplyModelPrices(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}

	var ov priceOverride
	if err := json.Unmarshal([]byte(s), &ov); err != nil {
		return fmt.Errorf("USAGE_MODEL_PRICES: invalid JSON: %w", err)
	}

	for vendorName, models := range ov {
		rates := make(map[string]vendors.Rates, len(models))
		for model, kinds := range models {
			r := make(vendors.Rates, len(kinds))
			for kind, micros := range kinds {
				r[vendors.Kind(kind)] = micros
			}
			rates[model] = r
		}
		if !vendors.MergePrices(vendorName, overrideVersion, rates) {
			return fmt.Errorf("USAGE_MODEL_PRICES: unknown vendor %q (register it before overriding its prices)", vendorName)
		}
	}
	return nil
}
