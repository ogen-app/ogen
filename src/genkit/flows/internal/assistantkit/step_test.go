package assistantkit

import "testing"

func TestStepOf(t *testing.T) {
	for name, tc := range map[string]struct {
		output any
		want   *Step
	}{
		"in the result": {map[string]any{"status": "ok", "result": map[string]any{"step": map[string]any{"subject": "Brochure", "passages": float64(3)}}},
			&Step{Subject: "Brochure", Passages: 3}},
		"at the top": {map[string]any{"step": map[string]any{"workspace": true}}, &Step{Workspace: true}},
		"none":       {map[string]any{"status": "ok", "result": map[string]any{"n": float64(1)}}, nil},
		"empty":      {map[string]any{"step": map[string]any{}}, nil},
		"not a map":  {"text", nil},
		"a bad step": {map[string]any{"step": "x"}, nil},
	} {
		got := StepOf(tc.output)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("%s: step = %+v; want %+v", name, got, tc.want)
		}
	}
}
