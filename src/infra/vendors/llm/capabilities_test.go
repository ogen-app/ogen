package llm_test

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// TestEveryAnthropicSlotAcceptsBothClaudeFamilies guards that a tier can run
// any Claude flow on either the 4.x or the 5.x family: every slot that allows
// Anthropic must accept every registered Claude chat model.
func TestEveryAnthropicSlotAcceptsBothClaudeFamilies(t *testing.T) {
	d, _ := vendors.Get(llm.VendorAnthropic)
	families := map[string]int{}
	for model, caps := range d.Capabilities {
		switch {
		case strings.Contains(model, "-4-"):
			families["4.x"]++
		case strings.Contains(model, "-5"):
			families["5.x"]++
		}
		for _, f := range modelconfig.Flows() {
			for _, s := range f.Slots {
				if !s.AllowsVendor(llm.VendorAnthropic) || !caps.Supports(s.Capability) {
					continue
				}
				if unmet := modelconfig.Satisfies(caps, s.Requires); len(unmet) != 0 {
					t.Errorf("%s/%s rejects %s: %v", f.Key, s.Key, model, unmet)
				}
			}
		}
	}
	if families["4.x"] == 0 || families["5.x"] == 0 {
		t.Fatalf("registry must carry both Claude families, got %v", families)
	}
}

// TestSeedDefaultsAreAssignable guards the code seeds: each slot's default must
// be a registered model its slot would accept through SetSlotModel, or a fresh
// database would start on a model the resolver can't run. Embed is seeded from
// EMBED_MODEL at boot, so it has no code default.
func TestSeedDefaultsAreAssignable(t *testing.T) {
	for _, f := range modelconfig.Flows() {
		for _, s := range f.Slots {
			model := modelconfig.SeedDefaults.For(f.Key, s.Key)
			if f.Key == modelconfig.FlowEmbed {
				if model != "" {
					t.Errorf("embed seed = %q, want empty (seeded from EMBED_MODEL)", model)
				}
				continue
			}
			vendor, ok := vendors.VendorOf(model)
			if !ok {
				t.Errorf("%s/%s: seed %q is not registered", f.Key, s.Key, model)
				continue
			}
			caps, _ := vendors.CapabilitiesOf(vendor, model)
			if !s.AllowsVendor(vendor) || !caps.Supports(s.Capability) {
				t.Errorf("%s/%s: seed %q (%s) not allowed for the slot", f.Key, s.Key, model, vendor)
				continue
			}
			if unmet := modelconfig.Satisfies(caps, s.Requires); len(unmet) != 0 {
				t.Errorf("%s/%s: seed %q misses %v", f.Key, s.Key, model, unmet)
			}
		}
	}
}

// TestEveryPricedModelHasCapabilities guards §8a: any model added to a model
// vendor's price table must also declare capabilities, otherwise ListModels
// would surface a model with an empty capability family that no slot could
// accept — an easy omission when adding a model.
func TestEveryPricedModelHasCapabilities(t *testing.T) {
	for _, d := range vendors.ByFamily(vendors.FamilyModel) {
		for model := range d.Prices.Models {
			caps, ok := vendors.CapabilitiesOf(d.Name, model)
			if !ok {
				t.Errorf("%s/%s: priced but no capabilities declared", d.Name, model)
				continue
			}
			if caps.Capability == "" {
				t.Errorf("%s/%s: capabilities missing Capability family", d.Name, model)
			}
		}
	}
}
