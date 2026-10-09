package vendors

import (
	"reflect"
	"testing"
)

// clearRegistry resets the process-global registry. Defined in the test
// file so production code carries no reset hook. Registry tests do not run
// in parallel, so sequential clear-then-register is safe.
func clearRegistry() {
	mu.Lock()
	registry = map[string]Descriptor{}
	mu.Unlock()
}

type fakeMeter struct{}

func (fakeMeter) Extract(any) (string, Usage, bool) { return "", nil, false }

func TestRegister_GetRoundTrip(t *testing.T) {
	clearRegistry()
	d := Descriptor{Name: "acme", Family: FamilyModel}
	Register(d)

	got, ok := Get("acme")
	if !ok {
		t.Fatal("Get(acme) not found after Register")
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("Get returned %+v, want %+v", got, d)
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("Get(nope) unexpectedly found")
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	clearRegistry()
	Register(Descriptor{Name: "dup", Family: FamilyModel})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register(Descriptor{Name: "dup", Family: FamilyPublisher})
}

func TestRegister_ValidationPanics(t *testing.T) {
	cases := map[string]Descriptor{
		"empty name":   {Family: FamilyModel},
		"empty family": {Name: "x"},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			clearRegistry()
			defer func() {
				if recover() == nil {
					t.Fatalf("Register(%s) did not panic", name)
				}
			}()
			Register(d)
		})
	}
}

func TestRegister_MeteredWithMeterOK(t *testing.T) {
	clearRegistry()
	Register(Descriptor{Name: "m", Family: FamilyModel, Metered: true, Meter: fakeMeter{}})
	if _, ok := Get("m"); !ok {
		t.Fatal("metered vendor with Meter should register")
	}
}

func TestRegister_MeteredWithoutMeterOK(t *testing.T) {
	// Publishers are metered but build events directly (no Meter) — must register.
	clearRegistry()
	Register(Descriptor{Name: "pub", Family: FamilyPublisher, Metered: true})
	if _, ok := Get("pub"); !ok {
		t.Fatal("metered publisher without Meter should register")
	}
}

func TestAll_SortedByName(t *testing.T) {
	clearRegistry()
	Register(Descriptor{Name: "zebra", Family: FamilyModel})
	Register(Descriptor{Name: "alpha", Family: FamilyPublisher})
	Register(Descriptor{Name: "mike", Family: FamilyModel})

	all := All()
	gotNames := []string{all[0].Name, all[1].Name, all[2].Name}
	want := []string{"alpha", "mike", "zebra"}
	if !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("All order = %v, want %v", gotNames, want)
	}
}

func TestByFamily(t *testing.T) {
	clearRegistry()
	Register(Descriptor{Name: "anthropic", Family: FamilyModel})
	Register(Descriptor{Name: "gemini", Family: FamilyModel})
	Register(Descriptor{Name: "zernio", Family: FamilyPublisher})

	models := ByFamily(FamilyModel)
	if len(models) != 2 || models[0].Name != "anthropic" || models[1].Name != "gemini" {
		t.Fatalf("ByFamily(model) = %+v, want [anthropic gemini]", models)
	}
	pubs := ByFamily(FamilyPublisher)
	if len(pubs) != 1 || pubs[0].Name != "zernio" {
		t.Fatalf("ByFamily(publisher) = %+v, want [zernio]", pubs)
	}
}

func TestCostOf(t *testing.T) {
	clearRegistry()
	Register(Descriptor{
		Name:   "anthropic",
		Family: FamilyModel,
		Prices: PriceTable{
			Version: "anthropic-2026-06",
			Models: map[string]Rates{
				"claude-sonnet": {KindInput: 3_000_000, KindOutput: 15_000_000},
			},
		},
	})

	t.Run("known model", func(t *testing.T) {
		micros, ver, ok := CostOf("anthropic", "claude-sonnet", Usage{KindInput: 1_000_000})
		if !ok || ver != "anthropic-2026-06" || micros != 3_000_000 {
			t.Fatalf("CostOf = (%d, %q, %v), want (3000000, anthropic-2026-06, true)", micros, ver, ok)
		}
	})
	t.Run("unknown vendor", func(t *testing.T) {
		micros, ver, ok := CostOf("openai", "gpt", Usage{KindInput: 100})
		if ok || ver != "" || micros != 0 {
			t.Fatalf("CostOf(unknown vendor) = (%d, %q, %v), want (0, \"\", false)", micros, ver, ok)
		}
	})
	t.Run("unknown model keeps version", func(t *testing.T) {
		micros, ver, ok := CostOf("anthropic", "claude-haiku", Usage{KindInput: 100})
		if ok || micros != 0 || ver != "anthropic-2026-06" {
			t.Fatalf("CostOf(unknown model) = (%d, %q, %v), want (0, anthropic-2026-06, false)", micros, ver, ok)
		}
	})
}

func TestMergePrices(t *testing.T) {
	clearRegistry()
	Register(Descriptor{
		Name:   "v",
		Family: FamilyModel,
		Prices: PriceTable{Version: "base", Models: map[string]Rates{
			"a": {KindInput: 1_000_000},
			"c": {KindInput: 7_000_000},
		}},
	})

	if !MergePrices("v", "ovr", map[string]Rates{
		"a": {KindInput: 5_000_000},  // override
		"b": {KindOutput: 2_000_000}, // add
	}) {
		t.Fatal("MergePrices returned false for a registered vendor")
	}

	d, _ := Get("v")
	if d.Prices.Version != "ovr" {
		t.Errorf("version = %q, want ovr", d.Prices.Version)
	}
	if d.Prices.Models["a"][KindInput] != 5_000_000 {
		t.Errorf("model a not overridden: %v", d.Prices.Models["a"])
	}
	if d.Prices.Models["b"][KindOutput] != 2_000_000 {
		t.Errorf("model b not added: %v", d.Prices.Models["b"])
	}
	if d.Prices.Models["c"][KindInput] != 7_000_000 {
		t.Errorf("model c (not in override) should survive the merge: %v", d.Prices.Models["c"])
	}

	if MergePrices("unregistered", "x", nil) {
		t.Error("MergePrices on an unregistered vendor should return false")
	}
}

// TestCostOf_SnapshotIsPure verifies cost is a pure function of (usage,
// rates): a cost computed against a captured rate set never changes if the
// vendor's registered prices are later edited. The recorder relies on this
// to store an immutable cost_micros snapshot.
func TestCostOf_SnapshotIsPure(t *testing.T) {
	clearRegistry()
	captured := Rates{KindInput: 3_000_000}
	u := Usage{KindInput: 1_000_000}
	snapshot := Cost(u, captured)

	Register(Descriptor{
		Name:   "anthropic",
		Family: FamilyModel,
		Prices: PriceTable{Version: "v2", Models: map[string]Rates{"m": {KindInput: 9_000_000}}},
	})
	// Registry now prices input 3x higher; the earlier snapshot is unmoved.
	if live, _, _ := CostOf("anthropic", "m", u); live != 9_000_000 {
		t.Fatalf("live CostOf = %d, want 9000000", live)
	}
	if snapshot != 3_000_000 {
		t.Fatalf("captured snapshot changed to %d, want 3000000", snapshot)
	}
}

func TestCostOf_LongPromptTier(t *testing.T) {
	clearRegistry()
	Register(Descriptor{
		Name:   "anthropic",
		Family: FamilyModel,
		Prices: PriceTable{
			Version: "v",
			Models: map[string]Rates{
				"tiered": {KindInput: 100_000, KindOutput: 500_000, KindCacheRead: 10_000},
				"flat":   {KindInput: 100_000, KindOutput: 500_000},
			},
			LongPrompt: map[string]PromptTier{
				"tiered": {Threshold: 100_000, Rates: Rates{KindInput: 500_000, KindOutput: 2_500_000, KindCacheRead: 50_000}},
			},
		},
	})

	tests := []struct {
		name  string
		model string
		u     Usage
		want  int64
	}{
		{
			name:  "at threshold stays on base rates",
			model: "tiered",
			u:     Usage{KindInput: 100_000, KindOutput: 1_000_000},
			want:  10_000 + 500_000,
		},
		{
			name:  "over threshold reprices the whole request, output included",
			model: "tiered",
			u:     Usage{KindInput: 100_001, KindOutput: 1_000_000},
			want:  50_000 + 2_500_000,
		},
		{
			name:  "cache reads count toward the prompt length",
			model: "tiered",
			u:     Usage{KindInput: 1_000, KindCacheRead: 99_001},
			want:  500 + 4950,
		},
		{
			name:  "cache writes count toward the prompt length",
			model: "tiered",
			u:     Usage{KindInput: 2, KindCacheCreation: 100_000},
			want:  1, // tier input rate; the tier has no cache_creation rate
		},
		{
			name:  "model without a tier ignores prompt length",
			model: "flat",
			u:     Usage{KindInput: 1_000_000},
			want:  100_000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := CostOf("anthropic", tc.model, tc.u)
			if !ok || got != tc.want {
				t.Fatalf("CostOf = (%d, %v), want (%d, true)", got, ok, tc.want)
			}
		})
	}
}

func TestMergePrices_KeepsLongPromptTiers(t *testing.T) {
	clearRegistry()
	tier := PromptTier{Threshold: 10, Rates: Rates{KindInput: 9}}
	Register(Descriptor{
		Name:   "v",
		Family: FamilyModel,
		Prices: PriceTable{
			Version:    "base",
			Models:     map[string]Rates{"a": {KindInput: 1}},
			LongPrompt: map[string]PromptTier{"a": tier},
		},
	})
	MergePrices("v", "ovr", map[string]Rates{"a": {KindInput: 2}})

	d, _ := Get("v")
	if !reflect.DeepEqual(d.Prices.LongPrompt["a"], tier) {
		t.Fatalf("long-prompt tier lost on merge: %+v", d.Prices.LongPrompt)
	}
}
