package campaign_assistant

import (
	"encoding/json"
	"testing"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// runTool calls a registered tool the way genkit does during a turn and
// decodes its outcome.
func runTool(t *testing.T, st *requestState, name string, input map[string]any) assistantkit.Outcome[json.RawMessage] {
	t.Helper()
	spec, ok := newTestTools(t).reg.Lookup(name)
	if !ok {
		t.Fatalf("tool %s is not registered", name)
	}
	tool, ok := spec.Tool.(ai.Tool)
	if !ok {
		t.Fatalf("tool %s is a %T, not an ai.Tool", name, spec.Tool)
	}
	raw, err := tool.RunRaw(withRequestState(t.Context(), st), input)
	if err != nil {
		t.Fatalf("%s returned an error, want an outcome: %v", name, err)
	}
	var out assistantkit.Outcome[json.RawMessage]
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s outcome %s: %v", name, b, err)
	}
	return out
}

// User-correctable input to the date and redistribute tools is refused with
// a reason instead of aborting the turn. None of these cases reach a
// repository, so the state needs none.
func TestToolRefusals(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		input  map[string]any
		reason string
	}{
		{"no dates given", "setCampaignDates", map[string]any{}, "give a new start or end date"},
		{"bad start", "setCampaignDates", map[string]any{"startDate": "next monday"}, "startDate must be an ISO date (YYYY-MM-DD)"},
		{"start after end", "setCampaignDates", map[string]any{"startDate": "2026-02-10", "endDate": "2026-02-01"}, "the start date must be before the end date"},
		{"redistribute without dates", "redistributePosts", map[string]any{}, "set the campaign's start and end dates first, then I can redistribute the posts"},
		{"empty asset question", "askCampaignAssets", map[string]any{"query": "  "}, "ask a question about the campaign's assets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := timelineCampaign()
			if tc.tool == "redistributePosts" {
				c.StartDate, c.EndDate = nil, nil
			}
			out := runTool(t, &requestState{campaignID: "c1", campaign: c}, tc.tool, tc.input)
			if out.Status != assistantkit.StatusRefused || out.Reason != tc.reason {
				t.Fatalf("outcome = %+v, want refused %q", out, tc.reason)
			}
		})
	}
}
