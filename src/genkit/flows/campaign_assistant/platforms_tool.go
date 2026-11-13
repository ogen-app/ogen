package campaign_assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/platforms"
)

// actionPlatformsUpdated labels a turn that added or removed platforms or
// post types.
const actionPlatformsUpdated = "platforms_updated"

// UpdateCampaignPlatformsInput lists the platforms (and post types) to add
// and to remove.
type UpdateCampaignPlatformsInput struct {
	Platforms []platforms.Request `json:"platforms,omitempty" jsonschema:"description=Platforms to add, each optionally with the post types to allow on it."`
	Remove    []platforms.Request `json:"remove,omitempty"    jsonschema:"description=Platforms to remove; with postTypes, only those post types stop being allowed on the platform."`
}

// UpdateCampaignPlatformsOutput reports what changed.
type UpdateCampaignPlatformsOutput struct {
	Added     string   `json:"added,omitempty"`
	Removed   string   `json:"removed,omitempty"`
	Platforms []string `json:"platforms"` // every platform the campaign now targets
	// PostsLeft is the unpublished posts left on what was removed; the
	// calendar check offers what to do with them.
	PostsLeft *PostsLeft `json:"postsLeft,omitempty"`
}

// PostsLeft counts the unpublished posts on removed platforms or post types.
type PostsLeft struct {
	Posts int `json:"posts"`
	// Scheduled still publish unless they are taken off the calendar.
	Scheduled int `json:"scheduled"`
}

// PlatformsResult summarises an updateCampaignPlatforms call.
type PlatformsResult struct {
	Added   string `json:"added,omitempty"`
	Removed string `json:"removed,omitempty"`
	// UndoID reverses the change via POST /api/assistant/actions/:id/undo;
	// empty when the change could not be logged.
	UndoID string `json:"undoId,omitempty"`
}

// PlatformsUpdatedEventPayload reports the platforms or post types added and
// removed.
type PlatformsUpdatedEventPayload struct {
	Added   string `json:"added,omitempty"`
	Removed string `json:"removed,omitempty"`
	UndoID  string `json:"undoId,omitempty"`
}

// platformsPlan is a request planned against the catalog: the campaign's
// platforms with it applied, and what it adds and removes.
type platformsPlan struct {
	next    models.CampaignPlatforms
	added   []platforms.Added
	removed []platforms.Removed
	catalog []models.Platform
}

// changes says what the plan does in one line, e.g. "Add Threads; remove
// the carousel post type on LinkedIn".
func (p platformsPlan) changes() string {
	var parts []string
	if len(p.added) > 0 {
		parts = append(parts, "add "+platforms.Summary(p.added))
	}
	if len(p.removed) > 0 {
		parts = append(parts, "remove "+platforms.RemovedSummary(p.removed))
	}
	out := strings.Join(parts, "; ")
	if out == "" {
		return ""
	}
	return strings.ToUpper(out[:1]) + out[1:]
}

// updatePlatformsGate always waits for a yes: a platform changes what every
// later generation may produce. A request the catalog can't satisfy, or one
// that changes nothing, is refused before anything is proposed.
var updatePlatformsGate = assistantkit.Gate[UpdateCampaignPlatformsInput]{
	Precheck: func(ctx context.Context, in UpdateCampaignPlatformsInput) error {
		_, err := planPlatforms(ctx, in)
		return err
	},
	// The card says what changes, and what a removal leaves behind.
	Describe: func(ctx context.Context, in UpdateCampaignPlatformsInput) string {
		plan, err := planPlatforms(ctx, in)
		if err != nil {
			return platformsSummary(in)
		}
		out := plan.changes()
		if left := postsLeft(ctx, plan.removed); left != nil {
			out += fmt.Sprintf(". %d unpublished %s stay on it", left.Posts, plural(left.Posts, "post", "posts"))
			if left.Scheduled > 0 {
				out += fmt.Sprintf(", %d scheduled and still publishing until taken off the calendar", left.Scheduled)
			}
		}
		return out
	},
}

// platformsSummary names the request when it can't be planned. A request
// without post types allows all of them, which also lifts the restriction
// on a platform the campaign already narrowed, so it says so rather than
// reading as a plain add.
func platformsSummary(in UpdateCampaignPlatformsInput) string {
	var parts []string
	if len(in.Platforms) > 0 {
		names := make([]string, len(in.Platforms))
		for i, r := range in.Platforms {
			if len(r.PostTypes) > 0 {
				names[i] = r.Platform + " (" + strings.Join(r.PostTypes, ", ") + ")"
			} else {
				names[i] = r.Platform + " (every post type)"
			}
		}
		parts = append(parts, "Add "+strings.Join(names, ", ")+" to the campaign's platforms")
	}
	for _, r := range in.Remove {
		if len(r.PostTypes) > 0 {
			parts = append(parts, "Stop allowing "+strings.Join(r.PostTypes, ", ")+" on "+r.Platform)
		} else {
			parts = append(parts, "Remove "+r.Platform+" from the campaign's platforms")
		}
	}
	return strings.Join(parts, "; ")
}

// postsLeft counts the campaign's unpublished posts on what removed takes
// away; nil when there are none, or they could not be read.
func postsLeft(ctx context.Context, removed []platforms.Removed) *PostsLeft {
	st := getRequestState(ctx)
	if len(removed) == 0 || st.repos.Posts == nil {
		return nil
	}
	posts, err := st.repos.Posts.ListRowsByCampaign(ctx, st.campaign.ID, "id", "status", "platform_id", "platform_post_type", "scheduled_at")
	if err != nil {
		return nil
	}
	left := &PostsLeft{}
	for _, p := range posts {
		if !unpublished(p.Status) || !slices.ContainsFunc(removed, func(r platforms.Removed) bool {
			return r.PlatformID == p.PlatformID && (len(r.PostTypes) == 0 || slices.Contains(r.PostTypes, p.PlatformPostType))
		}) {
			continue
		}
		left.Posts++
		if p.Status == models.PostStatusScheduled || p.Status == models.PostStatusScheduledForManualPublish {
			left.Scheduled++
		}
	}
	if left.Posts == 0 {
		return nil
	}
	return left
}

// unpublished reports whether a post in status may still publish.
func unpublished(s models.PostStatus) bool {
	switch s {
	case models.PostStatusPublished, models.PostStatusFailed, models.PostStatusNotPublished:
		return false
	}
	return true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// offerPlatforms turns "post on a platform the campaign doesn't target" into
// a stored offer to add it, so the user's "yes" adds it instead of the
// assistant asking again. It returns nil when every named platform is
// targeted already, or one isn't a platform Ogen publishes to at all, and the
// planner's refusal when a platform lacks the requested post type.
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
	plan, err := planRequest(ctx, in)
	// The platform exists but without that post type: adding it can't satisfy
	// the request, so the refusal that names its types is the useful answer.
	if ie, ok := errors.AsType[*platforms.InvalidError](err); ok && ie.PostType != "" {
		return assistantkit.Refused(ie.Reason, ie.Alternatives...)
	}
	if err != nil || len(plan.added) == 0 {
		return nil
	}
	summary := platforms.Summary(plan.added)
	return assistantkit.Conflicted(
		fmt.Sprintf("The campaign doesn't publish to %s yet.", summary),
		assistantkit.Option{ID: "add_platform", Title: "Add " + summary + " to the campaign", Recommended: true,
			Why: "What you asked for needs it, and the rest of the campaign stays as it is.", Tool: "updateCampaignPlatforms", Args: in},
	)
}

// planRequest checks the request against the enabled catalog and plans it:
// the additions first, then the removals. A request the catalog can't
// satisfy is a *platforms.InvalidError.
func planRequest(ctx context.Context, in UpdateCampaignPlatformsInput) (platformsPlan, error) {
	st := getRequestState(ctx)
	if st.repos.Platforms == nil {
		return platformsPlan{}, errors.New("the platform catalog is not available")
	}
	catalog, err := st.repos.Platforms.ListEnabled(ctx)
	if err != nil {
		return platformsPlan{}, fmt.Errorf("load platforms: %w", err)
	}
	next, added, err := platforms.Plan(st.campaign.TargetPlatforms, catalog, in.Platforms)
	if err != nil {
		return platformsPlan{}, err
	}
	next, removed, err := platforms.PlanRemoval(next, catalog, in.Remove)
	if err != nil {
		return platformsPlan{}, err
	}
	return platformsPlan{next: next, added: added, removed: removed, catalog: catalog}, nil
}

// planPlatforms is planRequest for the tool: an empty, unsatisfiable or
// no-op request is refused.
func planPlatforms(ctx context.Context, in UpdateCampaignPlatformsInput) (platformsPlan, error) {
	if len(in.Platforms) == 0 && len(in.Remove) == 0 {
		return platformsPlan{}, assistantkit.Refused("Say which platform to add or remove.")
	}
	plan, err := planRequest(ctx, in)
	if ie, ok := errors.AsType[*platforms.InvalidError](err); ok {
		return platformsPlan{}, assistantkit.Refused(ie.Reason, ie.Alternatives...)
	}
	if err != nil {
		return platformsPlan{}, err
	}
	if len(plan.added) == 0 && len(plan.removed) == 0 {
		return platformsPlan{}, assistantkit.Refused("The campaign's platforms are already like that.")
	}
	return plan, nil
}

// toolUpdateCampaignPlatforms adds and removes platforms or post types and
// logs the change so it can be undone. It writes only target_platforms, and
// the turn's campaign takes the change only once it is saved. Posts on what
// it removes stay as they are; it counts them for the user.
func toolUpdateCampaignPlatforms(ctx context.Context, in UpdateCampaignPlatformsInput) (*UpdateCampaignPlatformsOutput, error) {
	st := getRequestState(ctx)
	c := st.campaign
	plan, err := planPlatforms(ctx, in)
	if err != nil {
		return nil, err
	}
	next, catalog := plan.next, plan.catalog

	before := assistantundo.CampaignPlatforms(c)
	saved := &models.Campaign{ID: c.ID, TargetPlatforms: next}
	if err := st.repos.Campaigns.UpdateColumns(ctx, nil, saved, "target_platforms"); err != nil {
		return nil, fmt.Errorf("update campaign platforms: %w", err)
	}
	// Later tools in this turn resolve platforms from the hydrated list.
	hydrated := slices.DeleteFunc(slices.Clone(c.Platforms), func(p models.Platform) bool {
		return !slices.ContainsFunc(next, func(tp models.CampaignPlatform) bool { return tp.ID == p.ID })
	})
	for _, tp := range next {
		if i := slices.IndexFunc(catalog, func(p models.Platform) bool { return p.ID == tp.ID }); i >= 0 &&
			!slices.ContainsFunc(hydrated, func(p models.Platform) bool { return p.ID == tp.ID }) {
			hydrated = append(hydrated, catalog[i])
		}
	}
	st.mu.Lock()
	c.TargetPlatforms, c.Platforms, c.UpdatedAt = next, hydrated, saved.UpdatedAt
	st.mu.Unlock()

	added, removed := platforms.Summary(plan.added), platforms.RemovedSummary(plan.removed)
	st.platformsResult = &PlatformsResult{
		Added:   added,
		Removed: removed,
		UndoID:  st.logChange(ctx, models.AssistantActionCampaignPlatforms, before, assistantundo.CampaignPlatforms(c)),
	}
	st.recordWrite(actionPlatformsUpdated)
	names := make([]string, len(hydrated))
	for i, p := range hydrated {
		names[i] = p.Name
	}
	return &UpdateCampaignPlatformsOutput{Added: added, Removed: removed, Platforms: names, PostsLeft: postsLeft(ctx, plan.removed)}, nil
}

// platformsOutcome is the turn's result once platforms changed.
func platformsOutcome(r *PlatformsResult) outcome {
	var parts []string
	if r.Added != "" {
		parts = append(parts, fmt.Sprintf("Added %s to the campaign.", r.Added))
	}
	if r.Removed != "" {
		parts = append(parts, fmt.Sprintf("Removed %s from the campaign.", r.Removed))
	}
	return outcome{
		action:      actionPlatformsUpdated,
		explanation: strings.Join(parts, " "),
		attach:      func(resp *CampaignAssistantResponse) { resp.Platforms = r },
		event:       SSEEventPlatformsUpdated,
		payload:     PlatformsUpdatedEventPayload(*r),
	}
}
