package campaign_assistant

import (
	"fmt"
	"slices"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// The sub-flows report user-correctable input with their own ValidationError.
// These matchers turn it into a refusal the planner can relay; any other
// sub-flow error still aborts the turn.
var (
	contentPlanInvalid = assistantkit.As(func(e *content_plan.ValidationError) string { return e.Msg })
	enrichBriefInvalid = assistantkit.As(func(e *enrich_brief.ValidationError) string { return e.Msg })
	consistencyInvalid = assistantkit.As(func(e *consistency.ValidationError) string { return e.Msg })
)

// unsupportedPostType reports the first requested platform that cannot take
// postType, with the types it can take. An empty postType means the platform
// default and is always fine.
func unsupportedPostType(c *models.Campaign, platformIDs []string, postType string) (reason string, alternatives []string, bad bool) {
	if postType == "" {
		return "", nil, false
	}
	for _, id := range platformIDs {
		allowed := state.AllowedPostTypes(c, id)
		if len(allowed) == 0 || slices.Contains(allowed, postType) {
			continue
		}
		return fmt.Sprintf("%s in this campaign doesn't take %q posts.", platformName(c, id), postType), allowed, true
	}
	return "", nil, false
}

func platformName(c *models.Campaign, id string) string {
	for _, p := range c.Platforms {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}
