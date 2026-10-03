package campaign_assistant

import (
	"encoding/json"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
)

// plannerTurn reduces a stored model turn to the action + explanation envelope
// the planner itself emits, so tool results attached by the server are not
// echoed back into its own output. Plain-text rows pass through unchanged.
func plannerTurn(content string) string {
	var env struct {
		Action      string `json:"action"`
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return content
	}
	out, err := json.Marshal(env)
	if err != nil {
		return content
	}
	return string(out)
}

// NormalizeHistory rewrites, in place, model turns stored in the older flat
// shape so every row has the shape of the "complete" event.
func NormalizeHistory(msgs []models.CampaignAssistantMessage) {
	for i := range msgs {
		if msgs[i].Role == flowkit.RoleModel {
			msgs[i].Content = normalizeStoredTurn(msgs[i].Content)
		}
	}
}

// normalizeStoredTurn maps the flat {"briefApplied":true,"postCount":N} onto
// {"brief":{"applied":true},"contentPlan":{"postCount":N}}. Anything else is
// returned unchanged.
func normalizeStoredTurn(content string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &fields); err != nil {
		return content
	}
	applied, hasApplied := fields["briefApplied"]
	count, hasCount := fields["postCount"]
	if !hasApplied && !hasCount {
		return content
	}
	delete(fields, "briefApplied")
	delete(fields, "postCount")
	if _, ok := fields["brief"]; hasApplied && !ok {
		fields["brief"] = json.RawMessage(`{"applied":` + string(applied) + `}`)
	}
	if _, ok := fields["contentPlan"]; hasCount && !ok {
		fields["contentPlan"] = json.RawMessage(`{"postCount":` + string(count) + `}`)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return content
	}
	return string(out)
}
