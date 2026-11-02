package models

import "testing"

func TestEvaluationResultScanReadsLegacyShape(t *testing.T) {
	legacy := `{"clarity":{"score":6,"rationale":"Old rationale.","weakness":"w",
		"suggestions":[{"dimension":"clarity","severity":"high","issue":"Too long.","fix":"Cut it.","span":"x"}]}}`
	var r EvaluationResult
	if err := r.Scan(legacy); err != nil {
		t.Fatal(err)
	}
	if r.Clarity.Verdict != "Old rationale." {
		t.Errorf("verdict = %q", r.Clarity.Verdict)
	}
	if got := r.Clarity.Suggestions[0].Advice; got != "Too long. Cut it." {
		t.Errorf("advice = %q", got)
	}
}

func TestEvaluationResultScanPrefersCurrentFields(t *testing.T) {
	cur := `{"delivery":{"score":8,"verdict":"New.","rationale":"Old.",
		"suggestions":[{"severity":"low","span":"s","advice":"Do this.","issue":"i","fix":"f"}]}}`
	var r EvaluationResult
	if err := r.Scan([]byte(cur)); err != nil {
		t.Fatal(err)
	}
	if r.Delivery.Verdict != "New." || r.Delivery.Suggestions[0].Advice != "Do this." {
		t.Errorf("current fields should win: %+v", r.Delivery)
	}
}
