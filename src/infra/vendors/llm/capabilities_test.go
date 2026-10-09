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
