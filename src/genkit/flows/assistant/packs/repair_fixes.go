package packs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/reschedule"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
	"github.com/ogen-app/ogen/src/usecase/post_actions/withdraw"
)

// PostRemover deletes and unschedules posts the way the post endpoints do.
type PostRemover interface {
	// Delete deletes a post and its files; false when it was gone already.
	// A published post is withdraw.ErrPublished.
	Delete(ctx context.Context, post *models.Post, actor string) (bool, error)
	// Cancel withdraws a scheduled post from publishing; it goes back to
	// ready to publish, keeping its date.
	Cancel(ctx context.Context, postID, actor string) error
}

// PostsRef names some of a campaign's posts.
type PostsRef struct {
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
	PostIDs    []string `json:"postIds"`
}

// ChangePostTypeInput is posts and the post type to give them.
type ChangePostTypeInput struct {
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
	PostIDs    []string `json:"postIds"`
	PostType   string   `json:"postType" jsonschema:"description=A post type slug the campaign allows on the posts' platform"`
}

// PostFixOutput is what a fix did to the posts, and the ones it left.
type PostFixOutput struct {
	Changed int `json:"changed"`
	// Withdrawing counts scheduled posts being withdrawn from publishing.
	Withdrawing int `json:"withdrawing,omitzero"`
	// Skipped are the posts left as they were, each with why.
	Skipped []SkippedPost `json:"skipped,omitempty"`
	// Failed are the posts an error stopped, worth a retry.
	Failed []SkippedPost `json:"failed,omitempty"`
	// Posts are the posts changed or being withdrawn, each as it was and
	// as the fix left it, for the result card's rows.
	Posts  []ChangedPost `json:"posts"`
	UndoID string        `json:"undoId,omitempty"`
	assistantkit.Partial
}

// ChangedPost is one post a fix changed: its title and platform, and what
// changed. A field the fix didn't touch is left out.
type ChangedPost struct {
	PostID     string `json:"postId"`
	Title      string `json:"title"`
	PlatformID string `json:"platformId,omitempty"`
	Platform   string `json:"platform,omitempty"`
	// FromStatus and ToStatus are its status before and after; a withdrawn
	// post goes back to ready once its publishing job is cancelled, and a
	// deleted one has none after.
	FromStatus models.PostStatus `json:"fromStatus"`
	ToStatus   models.PostStatus `json:"toStatus,omitempty"`
	// WasAt is the date an unschedule took off it.
	WasAt *time.Time `json:"wasAt,omitempty"`
	// ScheduledAt is the date it keeps, when it has one.
	ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
	// FromType and ToType are a conversion's post types; Kept is what of
	// the post carried over: "caption", "media". Kept is left out when the
	// post's media couldn't be read, rather than claim it had none.
	FromType string   `json:"fromType,omitempty"`
	ToType   string   `json:"toType,omitempty"`
	Kept     []string `json:"kept,omitempty"`
}

// kept is what of p a conversion carries over: its caption and its media,
// media counting its attachments; nil when the count isn't known.
func kept(p models.Post, media int, known bool) []string {
	if !known && len(p.MediaURLs) == 0 {
		return nil
	}
	out := []string{}
	if strings.TrimSpace(p.Content) != "" {
		out = append(out, "caption")
	}
	if media > 0 || len(p.MediaURLs) > 0 {
		out = append(out, "media")
	}
	return out
}

// mediaCounts counts the posts' attachments by post. known is false when
// there is no way to count them or the read failed: the counts only dress
// a card, but no count must not read as none.
func mediaCounts(ctx context.Context, d Deps, ids []string) (counts map[string]int, known bool) {
	if len(ids) == 0 {
		return map[string]int{}, true
	}
	if d.Attachments == nil {
		return nil, false
	}
	atts, err := d.Attachments.ListByPostIDs(ctx, ids)
	if err != nil {
		return nil, false
	}
	counts = map[string]int{}
	for _, a := range atts {
		counts[a.PostID]++
	}
	return counts, true
}

// changed is p as a fix's row, before the fix.
func changed(c *models.Campaign, p models.Post) ChangedPost {
	cp := ChangedPost{PostID: p.ID, Title: p.Title, PlatformID: p.PlatformID, FromStatus: p.Status, ToStatus: p.Status}
	if i := slices.IndexFunc(c.Platforms, func(pl models.Platform) bool { return pl.ID == p.PlatformID }); i >= 0 {
		cp.Platform = c.Platforms[i].Name
	} else if p.Platform != nil {
		cp.Platform = p.Platform.Name
	}
	return cp
}

// failedAll is the error of a fix every post of which failed: nothing was
// done, so the run failed rather than did part of the job.
func (o *PostFixOutput) failedAll(first error) error {
	if first != nil && o.Changed == 0 && o.Withdrawing == 0 {
		return first
	}
	return nil
}

// postRetry reads a post fix: the posts it skipped, and the ones an error
// stopped, which the retried call takes again.
func postRetry(in PostsRef, out *PostFixOutput) (PostsRef, []assistantkit.NotMade, int, bool) {
	var notMade []assistantkit.NotMade
	var ids []string
	for _, p := range out.Skipped {
		notMade = append(notMade, assistantkit.NotMade{ID: p.PostID, Reason: p.Reason})
	}
	for _, p := range out.Failed {
		notMade = append(notMade, assistantkit.NotMade{ID: p.PostID, Reason: p.Reason})
		ids = append(ids, p.PostID)
	}
	in.PostIDs = ids
	return in, notMade, out.Changed + out.Withdrawing, len(ids) > 0
}

// SkippedPost is a post a fix left, and why.
type SkippedPost struct {
	PostID string `json:"postId"`
	Reason string `json:"reason"`
}

// postFixSpecs are the proposal-tier tools a finding's fixes run: taking
// posts off the calendar, deleting them, and changing their post type.
func postFixSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	unschedule := genkit.DefineTool(g, "unschedulePosts",
		"Takes posts off the calendar: an unscheduled post loses its date, a scheduled one is withdrawn from publishing and goes back to ready. It is offered for the user to confirm before it runs.",
		assistantkit.GatedRetry("unschedulePosts", assistantkit.Gate[PostsRef]{
			Recheck: recheckIDs(d, viewcontext.KindPost, campaignPostsIn, withPosts),
			Summary: func(in PostsRef) string {
				return fmt.Sprintf("Take %s off the calendar", plural(len(in.PostIDs), "post", "posts"))
			},
		}, postRetry, assistantkit.Wrap(recorded(repairPackName, "unschedulePosts", nil, func(ctx context.Context, in PostsRef) (*PostFixOutput, error) { return unschedulePosts(ctx, d, in) }))),
		ai.WithStrictSchema(false))
	changeType := genkit.DefineTool(g, "changePostType",
		"Changes posts to another post type the campaign allows on their platform; a post ready to publish goes back to draft to be checked again. It is offered for the user to confirm before it runs.",
		assistantkit.Gated("changePostType", assistantkit.Gate[ChangePostTypeInput]{
			Recheck: recheckIDs(d, viewcontext.KindPost, func(ctx context.Context, in ChangePostTypeInput) (string, []string) {
				return campaignPostsIn(ctx, PostsRef{CampaignID: in.CampaignID, PostIDs: in.PostIDs})
			}, func(in ChangePostTypeInput, ids []string) ChangePostTypeInput { in.PostIDs = ids; return in }),
			Summary: func(in ChangePostTypeInput) string {
				return fmt.Sprintf("Make %s %s", plural(len(in.PostIDs), "post", "posts"), in.PostType)
			},
		}, assistantkit.Wrap(recorded(repairPackName, "changePostType", nil, func(ctx context.Context, in ChangePostTypeInput) (*PostFixOutput, error) {
			return changePostType(ctx, d, in)
		}))), ai.WithStrictSchema(false))
	specs := []assistantkit.Spec{
		spec(repairPackName, unschedule, assistantkit.TierProposal,
			`Call this when the user asks to unschedule posts or take them off the calendar; pass the selected posts' ids when there is a selection. It is offered for the user to confirm. A finding's "take them off the calendar" fix is already this offer.`),
		spec(repairPackName, changeType, assistantkit.TierProposal,
			`Call this when the user asks to change posts to another post type ("make these image posts"); pass the post type's slug and the selected posts' ids. It is offered for the user to confirm. A finding's "make them …" fix is already this offer.`),
	}
	if d.Remove != nil {
		del := genkit.DefineTool(g, "deletePosts",
			"Deletes posts for good; published posts are kept. It is offered for the user to confirm before it runs.",
			assistantkit.GatedRetry("deletePosts", assistantkit.Gate[PostsRef]{
				Recheck: recheckIDs(d, viewcontext.KindPost, campaignPostsIn, withPosts),
				Summary: func(in PostsRef) string {
					return fmt.Sprintf("Delete %s; this can't be undone", plural(len(in.PostIDs), "post", "posts"))
				},
				Card: func(ctx context.Context, in PostsRef) *assistantkit.Card {
					c, posts, _, err := campaignPosts(ctx, d, in.CampaignID, in.PostIDs)
					if err != nil {
						return nil
					}
					return deleteCard(ctx, d, c, posts, in)
				},
			}, postRetry, assistantkit.Wrap(recorded(repairPackName, "deletePosts", nil, func(ctx context.Context, in PostsRef) (*PostFixOutput, error) { return deletePosts(ctx, d, in) }))),
			ai.WithStrictSchema(false))
		specs = append(specs, spec(repairPackName, del, assistantkit.TierProposal,
			`Call this when the user asks to delete posts; pass the selected posts' ids when there is a selection. It is offered for the user to confirm, and says it can't be undone. A finding's "delete them" fix is already this offer.`))
	}
	return specs
}

// withPosts narrows a post fix to ids.
func withPosts(in PostsRef, ids []string) PostsRef {
	in.PostIDs = ids
	return in
}

// campaignPosts loads the campaign and the named posts of it, in the order
// named; a post not in the campaign is skipped.
func campaignPosts(ctx context.Context, d Deps, campaignID string, ids []string) (*models.Campaign, []models.Post, []SkippedPost, error) {
	if len(ids) == 0 {
		return nil, nil, nil, assistantkit.Refused("which posts? pass postIds")
	}
	c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: campaignID})
	if err != nil {
		return nil, nil, nil, err
	}
	all, err := d.Posts.ListByCampaign(ctx, c.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	var posts []models.Post
	var skipped []SkippedPost
	for _, id := range ids {
		if i := slices.IndexFunc(all, func(p models.Post) bool { return p.ID == id }); i >= 0 {
			posts = append(posts, all[i])
		} else {
			skipped = append(skipped, SkippedPost{PostID: id, Reason: "not a post of this campaign any more"})
		}
	}
	return c, posts, skipped, nil
}

// unschedulePosts clears the date of the posts nothing is queued for, while
// they still have it, and withdraws the scheduled ones. Clearing the dates
// can be undone; a withdrawal can't.
func unschedulePosts(ctx context.Context, d Deps, in PostsRef) (*PostFixOutput, error) {
	c, posts, skipped, err := campaignPosts(ctx, d, in.CampaignID, in.PostIDs)
	if err != nil {
		return nil, err
	}
	out := &PostFixOutput{Skipped: skipped, Posts: []ChangedPost{}}
	var changes []repository.ScheduleChange
	var first error
	for _, p := range posts {
		switch {
		case p.Status == models.PostStatusScheduled && d.Remove != nil:
			if err := d.Remove.Cancel(ctx, p.ID, actor(ctx)); err != nil {
				first = cmp.Or(first, fmt.Errorf("withdraw post: %w", err))
				out.Failed = append(out.Failed, SkippedPost{PostID: p.ID, Reason: "withdrawing it failed: " + err.Error()})
				continue
			}
			out.Withdrawing++
			// A withdrawn post keeps its date and goes back to ready.
			row := changed(c, p)
			row.ToStatus, row.ScheduledAt = models.PostStatusReadyForPublish, p.ScheduledAt
			out.Posts = append(out.Posts, row)
		case reschedule.Eligible(p.Status) && p.ScheduledAt != nil:
			changes = append(changes, repository.ScheduleChange{PostID: p.ID, From: p.ScheduledAt})
		case p.ScheduledAt == nil:
			out.Skipped = append(out.Skipped, SkippedPost{PostID: p.ID, Reason: "not on the calendar"})
		default:
			out.Skipped = append(out.Skipped, SkippedPost{PostID: p.ID, Reason: "it is " + string(p.Status)})
		}
	}
	set, err := d.Posts.SetScheduleIf(ctx, nil, changes, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("unschedule posts: %w", err)
	}
	var before, after []assistantundo.PostState
	for _, ch := range changes {
		if !slices.Contains(set, ch.PostID) {
			out.Skipped = append(out.Skipped, SkippedPost{PostID: ch.PostID, Reason: "its date changed since the offer"})
			continue
		}
		before = append(before, assistantundo.PostState{ID: ch.PostID, ScheduledAt: ch.From})
		after = append(after, assistantundo.PostState{ID: ch.PostID})
		row := changed(c, posts[slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == ch.PostID })])
		row.WasAt = ch.From
		out.Posts = append(out.Posts, row)
	}
	out.Changed = len(set)
	if len(set) > 0 {
		out.UndoID = recordUndo(ctx, d, models.AssistantActionPostSchedules, c.ID, assistantundo.Posts(before), assistantundo.Posts(after))
	}
	if len(set) > 0 || out.Withdrawing > 0 {
		assistantkit.Touch(ctx, "post", "")
	}
	if err := out.failedAll(first); err != nil {
		return nil, err
	}
	return out, nil
}

// deletePosts deletes the posts, files and Zernio copies with them; a
// published post is kept.
func deletePosts(ctx context.Context, d Deps, in PostsRef) (*PostFixOutput, error) {
	c, posts, skipped, err := campaignPosts(ctx, d, in.CampaignID, in.PostIDs)
	if err != nil {
		return nil, err
	}
	out := &PostFixOutput{Skipped: skipped, Posts: []ChangedPost{}}
	var first error
	for _, p := range posts {
		if p.Status == models.PostStatusPublished {
			out.Skipped = append(out.Skipped, SkippedPost{PostID: p.ID, Reason: "it is published"})
			continue
		}
		deleted, err := d.Remove.Delete(ctx, &p, actor(ctx))
		switch {
		case errors.Is(err, withdraw.ErrPublished):
			out.Skipped = append(out.Skipped, SkippedPost{PostID: p.ID, Reason: "it was published since"})
		case err != nil:
			first = cmp.Or(first, fmt.Errorf("delete post: %w", err))
			out.Failed = append(out.Failed, SkippedPost{PostID: p.ID, Reason: "deleting it failed: " + err.Error()})
		case deleted:
			out.Changed++
			row := changed(c, p)
			row.ToStatus = ""
			out.Posts = append(out.Posts, row)
		}
	}
	if out.Changed > 0 {
		assistantkit.Touch(ctx, "post", "")
	}
	if err := out.failedAll(first); err != nil {
		return nil, err
	}
	return out, nil
}

// changePostType gives the posts a post type their platform allows on the
// campaign, while they still have the type and status they were offered
// with. A submitted post, a thread, or a type whose content doesn't carry
// over (a thread, a link post) is left alone.
func changePostType(ctx context.Context, d Deps, in ChangePostTypeInput) (*PostFixOutput, error) {
	if in.PostType == models.PostTypeThread || in.PostType == models.PostTypeLinkPost {
		return nil, assistantkit.Refused("a post becomes a " + in.PostType + " by hand: its content has to change with it")
	}
	c, posts, skipped, err := campaignPosts(ctx, d, in.CampaignID, in.PostIDs)
	if err != nil {
		return nil, err
	}
	out := &PostFixOutput{Skipped: skipped, Posts: []ChangedPost{}}
	var changes []repository.PostTypeChange
	for _, p := range posts {
		reason := ""
		switch {
		case !reschedule.Eligible(p.Status):
			reason = "it is " + string(p.Status) + "; unschedule it first"
		case p.PlatformPostType == models.PostTypeThread:
			reason = "a thread is rewritten by hand"
		case p.PlatformPostType == in.PostType:
			reason = "it is " + in.PostType + " already"
		case !slices.Contains(state.AllowedPostTypes(c, p.PlatformID), in.PostType):
			reason = "the campaign doesn't allow " + in.PostType + " on its platform"
		}
		if reason != "" {
			out.Skipped = append(out.Skipped, SkippedPost{PostID: p.ID, Reason: reason})
			continue
		}
		to := p.Status
		if to == models.PostStatusReadyForPublish {
			to = models.PostStatusDraft
		}
		changes = append(changes, repository.PostTypeChange{PostID: p.ID, From: p.PlatformPostType, To: in.PostType, FromStatus: p.Status, ToStatus: to})
	}
	set, err := d.Posts.SetPostTypeIf(ctx, nil, changes, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("change post types: %w", err)
	}
	var before, after []assistantundo.PostState
	media, known := mediaCounts(ctx, d, set)
	for _, ch := range changes {
		if !slices.Contains(set, ch.PostID) {
			out.Skipped = append(out.Skipped, SkippedPost{PostID: ch.PostID, Reason: "it changed since the offer"})
			continue
		}
		before = append(before, assistantundo.PostState{ID: ch.PostID, PostType: ch.From, Status: ch.FromStatus})
		after = append(after, assistantundo.PostState{ID: ch.PostID, PostType: ch.To, Status: ch.ToStatus})
		p := posts[slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == ch.PostID })]
		row := changed(c, p)
		row.ToStatus, row.ScheduledAt, row.FromType, row.ToType, row.Kept = ch.ToStatus, p.ScheduledAt, ch.From, ch.To, kept(p, media[p.ID], known)
		out.Posts = append(out.Posts, row)
	}
	out.Changed = len(set)
	if len(set) > 0 {
		out.UndoID = recordUndo(ctx, d, models.AssistantActionPostTypes, c.ID, assistantundo.Posts(before), assistantundo.Posts(after))
		assistantkit.Touch(ctx, "post", "")
	}
	return out, nil
}
