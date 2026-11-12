package assistantkit

import "encoding/json"

// Step is what a tool's line in the turn's steps says beyond the tool's
// name: what it read or made, and how much of it. A tool's result carries
// it as "step"; the runner lifts it onto the tool_result event, and the
// client words it.
type Step struct {
	// Subject names what the step read or made: an asset, a campaign, or
	// a campaign's lens.
	Subject string `json:"subject,omitempty"`
	// Workspace marks a step that read the workspace's lens.
	Workspace bool `json:"workspace,omitzero"`
	Passages  int  `json:"passages,omitzero"`
	Analyses  int  `json:"analyses,omitzero"`
	Sources   int  `json:"sources,omitzero"`
}

// StepOf reads the step a tool's JSON-decoded output carries, in its result
// or at its top; nil when it has none.
func StepOf(output any) *Step {
	m, ok := output.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["step"]
	if r, isMap := m["result"].(map[string]any); !ok && isMap {
		raw, ok = r["step"]
	}
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var s Step
	if json.Unmarshal(b, &s) != nil || s == (Step{}) {
		return nil
	}
	return &s
}
