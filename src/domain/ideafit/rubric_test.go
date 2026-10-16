package ideafit

import (
	"reflect"
	"strings"
	"testing"
)

// levels rates every criterion of the lens kind at level, then applies the
// overrides.
func levels(kind LensKind, hasPhases bool, level int, overrides map[string]int) map[string]Rating {
	out := map[string]Rating{}
	for _, c := range Criteria(kind, hasPhases) {
		out[c.Key] = Rating{Level: level}
	}
	for k, l := range overrides {
		if l == 0 {
			delete(out, k)
			continue
		}
		out[k] = Rating{Level: l}
	}
	return out
}

func TestRubricWeightsAndKeys(t *testing.T) {
	for _, kind := range []LensKind{LensCampaign, LensWorkspace} {
		total := 0
		seen := map[string]bool{}
		for _, c := range Criteria(kind, true) {
			if seen[c.Key] {
				t.Errorf("%s: duplicate key %q", kind, c.Key)
			}
			seen[c.Key] = true
			if c.Weight <= 0 || c.Question == "" {
				t.Errorf("%s/%s: weight %d, question %q", kind, c.Key, c.Weight, c.Question)
			}
			for i, l := range c.Levels {
				if l == "" {
					t.Errorf("%s/%s: level %d has no description", kind, c.Key, i+1)
				}
			}
			total += c.Weight
		}
		if len(seen) != 7 || total != 100 {
			t.Errorf("%s: %d criteria weighing %d, want 7 weighing 100", kind, len(seen), total)
		}
	}
}

func TestCriteriaWithoutPhases(t *testing.T) {
	got := Criteria(LensCampaign, false)
	if len(got) != 6 {
		t.Fatalf("got %d criteria, want 6", len(got))
	}
	for _, c := range got {
		if c.Key == "phase_fit" {
			t.Fatal("phase_fit requested for a campaign type without phases")
		}
	}
	if len(Criteria(LensCampaign, true)) != 7 {
		t.Fatal("dropping phase_fit changed the shared rubric")
	}
	if Criteria("other", true) != nil {
		t.Fatal("unknown lens kind has criteria")
	}
}

func TestCapsPerLens(t *testing.T) {
	if got := len(Caps(LensCampaign)); got != 3 {
		t.Errorf("campaign caps = %d, want 3", got)
	}
	for _, c := range Caps(LensWorkspace) {
		if c.Cap == CapOffBrief {
			t.Error("workspace lens offers off_brief")
		}
	}
}

func TestScore(t *testing.T) {
	tests := []struct {
		name     string
		kind     LensKind
		criteria map[string]Rating
		caps     []Cap
		want     *int
	}{
		{"campaign all 5", LensCampaign, levels(LensCampaign, true, 5, nil), nil, new(100)},
		{"campaign all 1", LensCampaign, levels(LensCampaign, true, 1, nil), nil, new(10)},
		{"campaign all 3", LensCampaign, levels(LensCampaign, true, 3, nil), nil, new(55)},
		// 25·1 + 20·.75 + 15·1 + 15·.5 + 10·.75 + 10·.5 + 5·.75 = 78.75 → 10 + 90·.7875 = 80.875
		{"campaign mixed", LensCampaign, levels(LensCampaign, true, 0, map[string]int{
			"message_fit": 5, "audience_fit": 4, "evidence": 5, "gap_fill": 3,
			"phase_fit": 4, "timeliness": 3, "brand_fit": 4,
		}), nil, new(81)},
		{"campaign without phases", LensCampaign, levels(LensCampaign, false, 5, nil), nil, new(100)},
		// (25·1 + 15·.5 + 15·.5) / 55 = .7273 → 10 + 90·.7273 = 75.45
		{"missing criteria leave the mean", LensCampaign, levels(LensCampaign, true, 0, map[string]int{
			"message_fit": 5, "evidence": 3, "gap_fill": 3,
		}), nil, new(75)},
		// exactly half the weight: (25·1 + 10·1 + 15·0) / 50 = .7 → 73
		{"half the weight is enough", LensCampaign, levels(LensCampaign, true, 0, map[string]int{
			"message_fit": 5, "timeliness": 5, "gap_fill": 1,
		}), nil, new(73)},
		{"under half the weight", LensCampaign, levels(LensCampaign, true, 0, map[string]int{
			"message_fit": 5, "audience_fit": 5,
		}), nil, nil},
		{"a cap pins a high score", LensCampaign, levels(LensCampaign, true, 5, nil), []Cap{CapGuardrailBreach}, new(CappedMax)},
		{"a cap keeps a low score", LensCampaign, levels(LensCampaign, true, 1, nil), []Cap{CapUnsupported}, new(10)},
		{"workspace all 5", LensWorkspace, levels(LensWorkspace, true, 5, nil), nil, new(100)},
		// 25·.75 + 20·.5 + 20·1 + 15·.25 + 10·0 + 5·1 + 5·.5 = 60 → 10 + 90·.6 = 64
		{"workspace mixed", LensWorkspace, levels(LensWorkspace, true, 0, map[string]int{
			"brand_relevance": 4, "audience_fit": 3, "evidence": 5, "gap_fill": 2,
			"campaign_potential": 1, "evergreen": 5, "brand_fit": 3,
		}), nil, new(64)},
		{"unknown lens", "other", levels(LensCampaign, true, 5, nil), nil, nil},
		{"no ratings", LensWorkspace, nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Score(Breakdown{Lens: tt.kind, Criteria: tt.criteria, Caps: tt.caps})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Score = %v, want %v", deref(got), deref(tt.want))
			}
		})
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestClean(t *testing.T) {
	long := strings.Repeat("é", maxWhyRunes+10)
	criteria, caps := Clean(LensCampaign, true, map[string]Rating{
		"message_fit": {Level: 9, Why: "  carries the key message  "},
		"evidence":    {Level: -2, Why: long},
		"gap_fill":    {Level: 0, Why: "left out"},
		"phase_fit":   {Level: 4, Phase: " Launch ", Campaign: "Spring"},
		"timeliness":  {Level: 3, Phase: "Launch"},
		"made_up":     {Level: 5},
	}, []Cap{"off_brief", " guardrail_breach ", "off_brief", "made_up"})

	want := map[string]Rating{
		"message_fit": {Level: 5, Why: "carries the key message"},
		"evidence":    {Level: 1, Why: strings.Repeat("é", maxWhyRunes)},
		"phase_fit":   {Level: 4, Phase: "Launch"},
		"timeliness":  {Level: 3},
	}
	if !reflect.DeepEqual(criteria, want) {
		t.Errorf("criteria = %+v, want %+v", criteria, want)
	}
	if want := []Cap{CapOffBrief, CapGuardrailBreach}; !reflect.DeepEqual(caps, want) {
		t.Errorf("caps = %v, want %v", caps, want)
	}

	criteria, caps = Clean(LensWorkspace, true, map[string]Rating{
		"campaign_potential": {Level: 4, Campaign: "Spring launch"},
		"message_fit":        {Level: 5},
	}, []Cap{CapOffBrief})
	if want := map[string]Rating{"campaign_potential": {Level: 4, Campaign: "Spring launch"}}; !reflect.DeepEqual(criteria, want) {
		t.Errorf("workspace criteria = %+v, want %+v", criteria, want)
	}
	if len(caps) != 0 {
		t.Errorf("workspace kept caps %v", caps)
	}

	criteria, _ = Clean(LensCampaign, false, map[string]Rating{
		"phase_fit": {Level: 4, Phase: "Launch"},
		"evidence":  {Level: 4},
	}, nil)
	if want := map[string]Rating{"evidence": {Level: 4}}; !reflect.DeepEqual(criteria, want) {
		t.Errorf("no-phases criteria = %+v, want phase_fit dropped", criteria)
	}

	criteria, caps = Clean(LensWorkspace, true, nil, nil)
	if criteria == nil || caps == nil {
		t.Error("Clean returned nil for empty input; the stored JSON needs {} and []")
	}
}

func TestBandOf(t *testing.T) {
	tests := []struct {
		score *int
		want  Band
	}{
		{nil, BandUnscored},
		{new(10), BandLow},
		{new(39), BandLow},
		{new(40), BandMedium},
		{new(69), BandMedium},
		{new(70), BandHigh},
		{new(100), BandHigh},
	}
	for _, tt := range tests {
		if got := BandOf(tt.score); got != tt.want {
			t.Errorf("BandOf(%v) = %s, want %s", deref(tt.score), got, tt.want)
		}
	}
}
