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
// default and is always fine. A platform with no types to choose from takes
// any type only when anyWhenNone is set: the draft engine keeps an explicit
// type there, the planning engine refuses it.
func unsupportedPostType(c *models.Campaign, platformIDs []string, postType string, anyWhenNone bool) (reason string, alternatives []string, bad bool) {
	if postType == "" {
		return "", nil, false
	}
	for _, id := range platformIDs {
		allowed := state.AllowedPostTypes(c, id)
		switch {
		case slices.Contains(allowed, postType):
			continue
		case len(allowed) == 0 && anyWhenNone:
			continue
		case len(allowed) == 0:
			return fmt.Sprintf("%s in this campaign has no post types to choose from; leave the post type out.", platformName(c, id)), nil, true
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
