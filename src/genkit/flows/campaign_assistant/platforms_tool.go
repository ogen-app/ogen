package campaign_assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/platforms"
)

// actionPlatformsUpdated labels a turn that added platforms or post types.
const actionPlatformsUpdated = "platforms_updated"

// UpdateCampaignPlatformsInput lists the platforms (and post types) to add.
type UpdateCampaignPlatformsInput struct {
	Platforms []platforms.Request `json:"platforms" jsonschema:"description=Platforms to add, each optionally with the post types to allow on it."`
}

// UpdateCampaignPlatformsOutput reports what was added.
type UpdateCampaignPlatformsOutput struct {
	Added     string   `json:"added"`
	Platforms []string `json:"platforms"` // every platform the campaign now targets
}

// PlatformsResult summarises an updateCampaignPlatforms call.
type PlatformsResult struct {
	Added string `json:"added"`
	// UndoID reverses the change via POST /api/assistant/actions/:id/undo;
	// empty when the change could not be logged.
	UndoID string `json:"undoId,omitempty"`
}

// PlatformsUpdatedEventPayload reports the added platforms or post types.
type PlatformsUpdatedEventPayload struct {
	Added  string `json:"added"`
	UndoID string `json:"undoId,omitempty"`
}

// updatePlatformsGate always waits for a yes: adding a platform changes what
// every later generation may produce. A request the catalog can't satisfy, or
// one that changes nothing, is refused before anything is proposed.
var updatePlatformsGate = assistantkit.Gate[UpdateCampaignPlatformsInput]{
	Precheck: func(ctx context.Context, in UpdateCampaignPlatformsInput) error {
		_, _, err := planPlatforms(ctx, in)
		return err
	},
	Summary: func(in UpdateCampaignPlatformsInput) string {
		names := make([]string, len(in.Platforms))
		for i, r := range in.Platforms {
			names[i] = r.Platform
			if len(r.PostTypes) > 0 {
				names[i] += " (" + strings.Join(r.PostTypes, ", ") + ")"
			}
		}
		return "Add " + strings.Join(names, ", ") + " to the campaign's platforms"
	},
}

// offerPlatforms turns "post on a platform the campaign doesn't target" into
// a stored offer to add it, so the user's "yes" adds it instead of the
// assistant asking again. It returns nil when every named platform is
// targeted already, or one isn't a platform Ogen publishes to at all.
func offerPlatforms(ctx context.Context, names []string, postType string) error {
	st := getRequestState(ctx)
	if st.repos.Platforms == nil {
		return nil
	}
	var missing []platforms.Request
	for _, n := range names {
		targeted := slices.ContainsFunc(st.campaign.Platforms, func(p models.Platform) bool {
			return p.ID == n || strings.EqualFold(p.Name, strings.TrimSpace(n))
		})
		if !targeted {
			r := platforms.Request{Platform: n}
			if postType != "" {
				r.PostTypes = []string{postType}
			}
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	in := UpdateCampaignPlatformsInput{Platforms: missing}
	added, _, err := planPlatforms(ctx, in)
	if err != nil || len(added) == 0 {
		return nil
	}
	summary := platforms.Summary(added)
	return assistantkit.Conflicted(
		fmt.Sprintf("The campaign doesn't publish to %s yet.", summary),
		assistantkit.Option{ID: "add_platform", Title: "Add " + summary + " to the campaign", Tool: "updateCampaignPlatforms", Args: in},
	)
}

// planPlatforms checks the request against the enabled catalog and returns
// the campaign's platforms with it applied, and the catalog.
func planPlatforms(ctx context.Context, in UpdateCampaignPlatformsInput) ([]platforms.Added, []models.Platform, error) {
	st := getRequestState(ctx)
	if st.repos.Platforms == nil {
		return nil, nil, errors.New("the platform catalog is not available")
	}
	if len(in.Platforms) == 0 {
		return nil, nil, assistantkit.Refused("Say which platform to add.")
	}
	catalog, err := st.repos.Platforms.ListEnabled(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load platforms: %w", err)
	}
	_, added, err := platforms.Plan(st.campaign.TargetPlatforms, catalog, in.Platforms)
	if ie, ok := errors.AsType[*platforms.InvalidError](err); ok {
		return nil, nil, assistantkit.Refused(ie.Reason, ie.Alternatives...)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(added) == 0 {
		return nil, nil, assistantkit.Refused("The campaign already has that.")
	}
	return added, catalog, nil
}

// toolUpdateCampaignPlatforms adds platforms or post types to the campaign
// and logs the change so it can be undone.
func toolUpdateCampaignPlatforms(ctx context.Context, in UpdateCampaignPlatformsInput) (*UpdateCampaignPlatformsOutput, error) {
	st := getRequestState(ctx)
	c := st.campaign
	added, catalog, err := planPlatforms(ctx, in)
	if err != nil {
		return nil, err
	}
	next, _, err := platforms.Plan(c.TargetPlatforms, catalog, in.Platforms)
	if err != nil {
		return nil, err
	}

	before := assistantundo.CampaignPlatforms(c)
	c.TargetPlatforms = next
	// Later tools in this turn resolve platforms from the hydrated list.
	for _, tp := range next {
		if i := slices.IndexFunc(catalog, func(p models.Platform) bool { return p.ID == tp.ID }); i >= 0 &&
			!slices.ContainsFunc(c.Platforms, func(p models.Platform) bool { return p.ID == tp.ID }) {
			c.Platforms = append(c.Platforms, catalog[i])
		}
	}
	c.UpdatedAt = time.Now().UTC()
	if err := st.repos.Campaigns.Update(ctx, c); err != nil {
		return nil, fmt.Errorf("update campaign platforms: %w", err)
	}

	summary := platforms.Summary(added)
	st.platformsResult = &PlatformsResult{
		Added:  summary,
		UndoID: st.logChange(ctx, models.AssistantActionCampaignPlatforms, before, assistantundo.CampaignPlatforms(c)),
	}
	st.recordWrite(actionPlatformsUpdated)
	names := make([]string, len(c.Platforms))
	for i, p := range c.Platforms {
		names[i] = p.Name
	}
	return &UpdateCampaignPlatformsOutput{Added: summary, Platforms: names}, nil
}

func platformsOutcome(r *PlatformsResult) outcome {
	return outcome{
		action:      actionPlatformsUpdated,
		explanation: fmt.Sprintf("Added %s to the campaign.", r.Added),
		attach:      func(resp *CampaignAssistantResponse) { resp.Platforms = r },
		event:       SSEEventPlatformsUpdated,
		payload:     PlatformsUpdatedEventPayload(*r),
	}
}
