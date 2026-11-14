package campaign_assistant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/platforms"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// actionPlatformsUpdated labels a turn that added or removed platforms or
// post types.
const actionPlatformsUpdated = "platforms_updated"

// What a removal does with the posts scheduled to publish on what it
// removes. The server sets it on the choice it offers.
const (
	// scheduledUnschedule withdraws them from publishing, then removes.
	scheduledUnschedule = "unschedule"
	// scheduledLater removes nothing until they are out.
	scheduledLater = "later"
)

// keptListed caps the posts a removal's result names one by one.
const keptListed = 10

// UpdateCampaignPlatformsInput lists the platforms (and post types) to add
// and to remove.
type UpdateCampaignPlatformsInput struct {
	Platforms []platforms.Request `json:"platforms,omitempty" jsonschema:"description=Platforms to add, each optionally with the post types to allow on it."`
	Remove    []platforms.Request `json:"remove,omitempty"    jsonschema:"description=Platforms to remove; with postTypes, only those post types stop being allowed on the platform."`
	// Scheduled is the user's pick when posts scheduled to publish on what
	// is removed held the removal back.
	Scheduled string `json:"scheduled,omitempty" jsonschema:"description=Leave empty: set by the server when the user picks how to handle posts scheduled on what is removed."`
	// Unschedule is the scheduled posts the choice offered to withdraw; a
	// post scheduled there since is not among them.
	Unschedule []string `json:"unschedule,omitempty" jsonschema:"description=Leave empty: set by the server with the posts the user agreed to unschedule."`
}

// UpdateCampaignPlatformsOutput reports what changed.
type UpdateCampaignPlatformsOutput struct {
	Added     string   `json:"added,omitempty"`
	Removed   string   `json:"removed,omitempty"`
	Platforms []string `json:"platforms"` // every platform the campaign now targets
	// PostsLeft is the unpublished posts left on what was removed; the
	// calendar check offers what to do with them.
	PostsLeft *PostsLeft `json:"postsLeft,omitempty"`
	// Unscheduled is the posts withdrawn from publishing first.
	Unscheduled int `json:"unscheduled,omitzero"`
	// Later is set when nothing was removed: the user chose to wait for the
	// scheduled posts to go out.
	Later *LaterResult `json:"later,omitempty"`
}

// LaterResult is a removal put off until the posts scheduled on it are out.
type LaterResult struct {
	Removed string `json:"removed"`
	// Until is when the last of them is scheduled, RFC 3339.
	Until string `json:"until,omitempty"`
}

// PostsLeft counts the unpublished posts on removed platforms or post types.
type PostsLeft struct {
	Posts int `json:"posts"`
	// Scheduled still publish unless they are taken off the calendar.
	Scheduled int `json:"scheduled"`
}

// KeptPost is a post a removal leaves where it was: it stays, flagged as
// outside the campaign's platforms or post types.
type KeptPost struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Platform string            `json:"platform"`
	PostType string            `json:"postType,omitempty"`
	Status   models.PostStatus `json:"status"`
}

// PlatformsResult summarises an updateCampaignPlatforms call.
type PlatformsResult struct {
	Added   string `json:"added,omitempty"`
	Removed string `json:"removed,omitempty"`
	// PostsLeft is set on a removal whose posts were read; zero counts mean
	// nothing was on it.
	PostsLeft *PostsLeft `json:"postsLeft,omitempty"`
	// Kept names up to keptListed of the posts left on what was removed;
	// PostsLeft.Posts counts them all.
	Kept []KeptPost `json:"kept,omitempty"`
	// Unscheduled is how many posts were withdrawn from publishing before
	// the removal. A withdrawal can't be undone.
	Unscheduled int `json:"unscheduled,omitzero"`
	// UndoID reverses the change via POST /api/assistant/actions/:id/undo;
	// empty when the change could not be logged.
	UndoID string `json:"undoId,omitempty"`
}

// PlatformsUpdatedEventPayload reports the platforms or post types added and
// removed.
type PlatformsUpdatedEventPayload struct {
	Added       string     `json:"added,omitempty"`
	Removed     string     `json:"removed,omitempty"`
	PostsLeft   *PostsLeft `json:"postsLeft,omitempty"`
	Kept        []KeptPost `json:"kept,omitempty"`
	Unscheduled int        `json:"unscheduled,omitzero"`
	UndoID      string     `json:"undoId,omitempty"`
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
//
// A post scheduled to publish on what a removal takes away holds it back,
// since it would publish where the campaign no longer is: the user chooses
// to unschedule it first or to remove later.
var updatePlatformsGate = assistantkit.Gate[UpdateCampaignPlatformsInput]{
	Precheck: func(ctx context.Context, in UpdateCampaignPlatformsInput) error {
		plan, err := planPlatforms(ctx, in)
		if err != nil {
			return err
		}
		// Only the choice's own options carry a pick, and they run
		// confirmed: an offer must never hide one its card doesn't show.
		if (in.Scheduled != "" || len(in.Unschedule) > 0) && !assistantkit.Confirmed(ctx) {
			return assistantkit.Refused("Leave scheduled and unschedule empty: they are set by the choice offered to the user.")
		}
		switch in.Scheduled {
		case "", scheduledLater:
		case scheduledUnschedule:
			if getRequestState(ctx).cancelPost == nil {
				return assistantkit.Refused("Scheduled posts can't be withdrawn from here.")
			}
		default:
			return assistantkit.Refused("Leave scheduled empty: the server sets it.")
		}
		if in.Scheduled != "" || len(plan.removed) == 0 {
			return nil
		}
		left, ok := removedPosts(ctx, plan.removed)
		if !ok {
			return errPostsUnread(plan.removed)
		}
		if blocking := publishing(left); len(blocking) > 0 {
			return scheduledConflict(ctx, in, plan, left, blocking)
		}
		return nil
	},
	// The card says what changes, and what a removal leaves behind.
	Describe: func(ctx context.Context, in UpdateCampaignPlatformsInput) string {
		plan, err := planPlatforms(ctx, in)
		if err != nil {
			return platformsSummary(in)
		}
		out := plan.changes()
		if left := postsLeft(ctx, plan.removed); left != nil && left.Posts > 0 {
			out += fmt.Sprintf(". %d unpublished %s stay on it", left.Posts, plural(left.Posts, "post", "posts"))
			if left.Scheduled > 0 {
				out += fmt.Sprintf(", %d scheduled and still publishing until taken off the calendar", left.Scheduled)
			}
		}
		return out
	},
	// A removal names its button and the posts that stay, flagged.
	Card: func(ctx context.Context, in UpdateCampaignPlatformsInput) *assistantkit.Card {
		plan, err := planPlatforms(ctx, in)
		if err != nil || len(plan.removed) == 0 {
			return nil
		}
		card := &assistantkit.Card{}
		if len(plan.added) == 0 {
			card.Verb = "Remove " + removedLabel(plan.removed)
		}
		if left, ok := removedPosts(ctx, plan.removed); ok && len(left) > 0 {
			card.Rows = []assistantkit.CardRow{{Kind: assistantkit.CardRowPosts,
				Value: fmt.Sprintf("%d unpublished %s on %s", len(left), plural(len(left), "post is", "posts are"), removedLabel(plan.removed)),
				Note:  "they stay, flagged “" + flagOf(plan.removed) + "”"}}
		}
		return card
	},
}

// removedLabel names what a removal takes away, short: "Facebook", "reel".
func removedLabel(removed []platforms.Removed) string {
	parts := make([]string, len(removed))
	for i, r := range removed {
		parts[i] = r.Name
		if len(r.PostTypes) > 0 {
			parts[i] = strings.Join(r.PostTypes, ", ")
		}
	}
	return strings.Join(parts, ", ")
}

// flagOf is the flag Quality puts on a post left on what was removed.
func flagOf(removed []platforms.Removed) string {
	if slices.ContainsFunc(removed, func(r platforms.Removed) bool { return len(r.PostTypes) == 0 }) {
		return "not a platform of this campaign"
	}
	return "not a post type of this campaign"
}

// errPostsUnread refuses a removal whose posts could not be read.
func errPostsUnread(removed []platforms.Removed) error {
	return assistantkit.Refused(fmt.Sprintf("The posts on %s couldn't be checked for scheduled ones, so nothing was removed. Try again in a moment.", removedLabel(removed)))
}

// publishing are the posts that publish on their own unless withdrawn.
func publishing(posts []models.Post) []models.Post {
	return slices.DeleteFunc(slices.Clone(posts), func(p models.Post) bool { return p.Status != models.PostStatusScheduled })
}

// scheduledConflict offers the choice a scheduled post forces on a removal:
// withdraw it and remove now, the recommended way, or remove later.
func scheduledConflict(ctx context.Context, in UpdateCampaignPlatformsInput, plan platformsPlan, left, blocking []models.Post) error {
	st := getRequestState(ctx)
	label := removedLabel(plan.removed)
	n := len(blocking)
	var opts []assistantkit.Option
	if st.cancelPost != nil {
		unschedule := in
		unschedule.Scheduled = scheduledUnschedule
		for _, p := range blocking {
			unschedule.Unschedule = append(unschedule.Unschedule, p.ID)
		}
		desc := fmt.Sprintf("%d posts scheduled up to %s go back to ready to publish.", n, when(st.campaign, lastScheduled(blocking)))
		if n == 1 {
			desc = fmt.Sprintf("“%s” is scheduled for %s and goes back to ready to publish.", titleOf(blocking[0]), when(st.campaign, blocking[0].ScheduledAt))
		}
		if rest := len(left) - n; rest > 0 {
			desc += fmt.Sprintf(" The %d other %s, flagged in Quality.", rest, plural(rest, "post stays", "posts stay"))
		}
		opts = append(opts, assistantkit.Option{ID: "unschedule", Title: fmt.Sprintf("Unschedule %d, then remove", n),
			Description: desc, Recommended: true,
			Why: "The campaign leaves the platform now, and no post publishes where it no longer is.", Tool: "updateCampaignPlatforms", Args: unschedule})
	}
	later := in
	later.Scheduled = scheduledLater
	opts = append(opts, assistantkit.Option{ID: "later", Title: "Remove later",
		Description: fmt.Sprintf("Keep %s until the scheduled %s out; ask again after %s.", label, plural(n, "post is", "posts are"), dayOf(st.campaign, lastScheduled(blocking))),
		Tool:        "updateCampaignPlatforms", Args: later})
	return assistantkit.ConflictedCard(
		fmt.Sprintf("%s has %d %s in this campaign", label, len(left), plural(len(left), "post", "posts")),
		&assistantkit.Card{Footnote: fmt.Sprintf("A post already scheduled to %s would publish where the campaign no longer is, so removal waits on it.", label)},
		opts...)
}

// lastScheduled is the latest date among posts.
func lastScheduled(posts []models.Post) *time.Time {
	var last *time.Time
	for _, p := range posts {
		if p.ScheduledAt != nil && (last == nil || p.ScheduledAt.After(*last)) {
			last = p.ScheduledAt
		}
	}
	return last
}

// when is t in the campaign's time zone, e.g. "Tue Apr 22, 09:00".
func when(c *models.Campaign, t *time.Time) string {
	if t == nil {
		return "a date not set"
	}
	return t.In(campaignLoc(c)).Format("Mon Jan 2, 15:04")
}

// dayOf is t's day in the campaign's time zone, e.g. "Tue Apr 22".
func dayOf(c *models.Campaign, t *time.Time) string {
	if t == nil {
		return "it is out"
	}
	return t.In(campaignLoc(c)).Format("Mon Jan 2")
}

// campaignLoc is the campaign's time zone, UTC when it has none.
func campaignLoc(c *models.Campaign) *time.Location {
	loc, err := settings.ResolveTimezone(c.Timezone)
	if err != nil || loc == nil {
		return time.UTC
	}
	return loc
}

// titleOf is a post's title, or a stand-in for an untitled one.
func titleOf(p models.Post) string {
	return cmp.Or(strings.TrimSpace(p.Title), "Untitled post")
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

// removedPosts are the campaign's unpublished posts on what removed takes
// away, by date; ok is false when there is no removal or they could not be
// read.
func removedPosts(ctx context.Context, removed []platforms.Removed) (left []models.Post, ok bool) {
	st := getRequestState(ctx)
	if len(removed) == 0 || st.repos.Posts == nil {
		return nil, false
	}
	posts, err := st.repos.Posts.ListRowsByCampaign(ctx, st.campaign.ID, "id", "title", "status", "platform_id", "platform_post_type", "scheduled_at")
	if err != nil {
		return nil, false
	}
	left = slices.DeleteFunc(posts, func(p models.Post) bool {
		return !unpublished(p.Status) || !slices.ContainsFunc(removed, func(r platforms.Removed) bool {
			return r.PlatformID == p.PlatformID && (len(r.PostTypes) == 0 || slices.Contains(r.PostTypes, p.PlatformPostType))
		})
	})
	return left, true
}

// countLeft counts the posts a removal leaves.
func countLeft(left []models.Post) *PostsLeft {
	out := &PostsLeft{Posts: len(left)}
	for _, p := range left {
		if p.Status == models.PostStatusScheduled || p.Status == models.PostStatusScheduledForManualPublish {
			out.Scheduled++
		}
	}
	return out
}

// postsLeft counts the campaign's unpublished posts on what removed takes
// away; zero counts when there are none, nil when there is no removal or
// they could not be read.
func postsLeft(ctx context.Context, removed []platforms.Removed) *PostsLeft {
	left, ok := removedPosts(ctx, removed)
	if !ok {
		return nil
	}
	return countLeft(left)
}

// keptPosts names up to keptListed of the posts a removal leaves.
func keptPosts(left []models.Post, removed []platforms.Removed) []KeptPost {
	var out []KeptPost
	for _, p := range left[:min(len(left), keptListed)] {
		k := KeptPost{ID: p.ID, Title: titleOf(p), PostType: p.PlatformPostType, Status: p.Status}
		if i := slices.IndexFunc(removed, func(r platforms.Removed) bool { return r.PlatformID == p.PlatformID }); i >= 0 {
			k.Platform = removed[i].Name
		}
		out = append(out, k)
	}
	return out
}

// unschedule withdraws the posts from publishing and returns how many were;
// a withdrawal that fails leaves the post scheduled and fails the call, so
// nothing is removed under it.
func unschedule(ctx context.Context, posts []models.Post) (int, error) {
	st := getRequestState(ctx)
	actor, _ := logging.UserIDFrom(ctx)
	n := 0
	for _, p := range posts {
		if err := st.cancelPost(ctx, p.ID, actor); err != nil {
			return n, fmt.Errorf("withdraw post %s: %w", p.ID, err)
		}
		n++
		assistantkit.Touch(ctx, "post", p.ID)
	}
	return n, nil
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
// it removes stay, flagged; it names them for the user. A post scheduled to
// publish there holds the removal back until the user picks: withdraw it
// first, or remove later, which writes nothing.
func toolUpdateCampaignPlatforms(ctx context.Context, in UpdateCampaignPlatformsInput) (*UpdateCampaignPlatformsOutput, error) {
	st := getRequestState(ctx)
	c := st.campaign
	plan, err := planPlatforms(ctx, in)
	if err != nil {
		return nil, err
	}
	next, catalog := plan.next, plan.catalog
	left, read := removedPosts(ctx, plan.removed)
	if len(plan.removed) > 0 && !read {
		// Without the posts, a scheduled one could publish where the
		// campaign no longer is.
		return nil, errPostsUnread(plan.removed)
	}
	blocking := publishing(left)
	unscheduled := 0
	switch {
	case in.Scheduled == scheduledLater:
		later := &LaterResult{Removed: platforms.RemovedSummary(plan.removed)}
		if t := lastScheduled(blocking); t != nil {
			later.Until = t.UTC().Format(time.RFC3339)
		}
		return &UpdateCampaignPlatformsOutput{Later: later}, nil
	case len(blocking) > 0 && (in.Scheduled == "" || slices.ContainsFunc(blocking, func(p models.Post) bool { return !slices.Contains(in.Unschedule, p.ID) })):
		// A post was scheduled there after the offer: what to do with it
		// is the user's choice, not this offer's.
		return nil, assistantkit.Refused(fmt.Sprintf("%sa post was scheduled on %s since, so nothing was withdrawn or removed. Ask again to choose what happens to it.",
			assistantkit.ChangedSince, removedLabel(plan.removed)))
	case len(blocking) > 0:
		n, err := unschedule(ctx, blocking)
		if err != nil {
			slog.WarnContext(ctx, "withdraw before platform removal failed", logging.AttrComponent, logComponent, logging.AttrError, err)
			return nil, assistantkit.Refused(fmt.Sprintf("Withdrawing the scheduled posts failed after %d of %d, so nothing was removed.", n, len(blocking)))
		}
		unscheduled = n
		for i := range left {
			if left[i].Status == models.PostStatusScheduled {
				left[i].Status = models.PostStatusReadyForPublish
			}
		}
	}

	before := assistantundo.CampaignPlatforms(c)
	saved := &models.Campaign{ID: c.ID, TargetPlatforms: next}
	if err := st.repos.Campaigns.UpdateColumns(ctx, nil, saved, "target_platforms"); err != nil {
		return nil, fmt.Errorf("update campaign platforms after withdrawing %d scheduled posts: %w", unscheduled, err)
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
	r := &PlatformsResult{
		Added:       added,
		Removed:     removed,
		Unscheduled: unscheduled,
		UndoID:      st.logChange(ctx, models.AssistantActionCampaignPlatforms, before, assistantundo.CampaignPlatforms(c)),
	}
	if read {
		r.PostsLeft, r.Kept = countLeft(left), keptPosts(left, plan.removed)
	}
	st.platformsResult = r
	st.recordWrite(actionPlatformsUpdated)
	names := make([]string, len(hydrated))
	for i, p := range hydrated {
		names[i] = p.Name
	}
	return &UpdateCampaignPlatformsOutput{Added: added, Removed: removed, Platforms: names, PostsLeft: r.PostsLeft, Unscheduled: unscheduled}, nil
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
