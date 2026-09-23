package llm_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/infra/vendors"
	_ "github.com/ogen-app/ogen/src/infra/vendors/llm" // register the model vendors
)

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
