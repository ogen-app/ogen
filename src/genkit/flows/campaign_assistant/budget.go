package campaign_assistant

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// maxGoalPerPeriod bounds the posts per period a raise offers.
const maxGoalPerPeriod = 100

// draftOverGoal returns a conflict when drafting in's posts would take the
// campaign past its goal. The ways forward, the recommended one first:
// fill the goal exactly (when there is room and the call drafts for one
// platform), replace the weakest drafts (when enough have a quality score),
// save them as ideas, which fill no goal, or raise the goal to fit them.
// When the posts could not be counted the room is unknown: saving them as
// ideas is recommended, and drafting them without the check waits for a
// yes.
func draftOverGoal(ctx context.Context, in DraftPostInput) error {
	st := getRequestState(ctx)
	g := st.snapshot.Goal
	count := draftTotal(ctx, in)
	ideasDesc := fmt.Sprintf("Nothing drafted; the %d wait in Ideas and don't count toward the goal.", count)
	if in.PublishDate != "" {
		ideasDesc = fmt.Sprintf("Nothing drafted; the %d wait in Ideas with %s as their target, and don't count toward the goal.", count, in.PublishDate)
	}
	ideas := assistantkit.Option{ID: "ideas", Title: fmt.Sprintf("Save the %d as ideas instead", count),
		Description: ideasDesc, Tool: "generatePosts", Args: ideasOf(in)}
	if g != nil && g.Unknown {
		ideas.Recommended, ideas.Why = true, "Ideas don't count toward the goal, so nothing can take the campaign past it."
		reason := fmt.Sprintf("The campaign's posts could not be read, so it is unclear how much room is left under its goal of %d.", g.Target)
		return assistantkit.ConflictedCard(reason,
			&assistantkit.Card{Footnote: "Replace isn't offered: without the campaign's posts there are no drafts to rank."},
			ideas,
			assistantkit.Option{ID: "anyway", Title: fmt.Sprintf("Draft %d without checking the goal", count),
				Description: "Draft them all, and check the goal once the posts can be read.", Tool: "draftPost", Args: in})
	}
	if g == nil || count <= g.Room {
		return nil
	}
	reason := fmt.Sprintf("That would add %d posts, but the goal is %d and the campaign already has %d, so there is room for %d more.",
		count, g.Target, g.Planned, g.Room)
	cadence := cmp.Or(st.campaign.GoalCadence, campaigngoal.DefaultCadence)
	var opts []assistantkit.Option
	if g.Room > 0 && draftPlatforms(ctx, in) == 1 {
		fill := in
		fill.Count = g.Room
		opts = append(opts, assistantkit.Option{ID: "fill", Title: fmt.Sprintf("Draft %d to reach the goal", g.Room),
			Description: fmt.Sprintf("The other %d aren't drafted, and the campaign reaches its goal of %d.", count-g.Room, g.Target),
			Tool:        "draftPost", Args: fill})
	}
	var card *assistantkit.Card
	if weakest := weakestDrafts(ctx, count); len(weakest) == count {
		ids := make([]string, len(weakest))
		for i, p := range weakest {
			ids[i] = p.ID
		}
		opts = append(opts, assistantkit.Option{ID: "replace",
			Title: fmt.Sprintf("Replace the %d weakest drafts with them", count),
			Description: fmt.Sprintf("Draft the %d and delete for good the %d lowest-scored drafts that aren't scheduled. The campaign stays at %d posts.",
				count, count, g.Planned),
			Tool: "replaceWeakestDrafts", Args: ReplaceDraftsInput{Delete: ids, Draft: in}})
	} else if st.deletePost != nil {
		card = &assistantkit.Card{Footnote: fmt.Sprintf("Replace isn't offered: only %d %s a quality score to rank against the %d new ones.",
			len(weakest), plural(len(weakest), "draft has", "drafts have"), count)}
	}
	opts = append(opts, ideas)
	if per := raisedGoal(st.campaign, g.Planned+count); per > 0 {
		opts = append(opts, assistantkit.Option{ID: "raise",
			Title:       fmt.Sprintf("Raise the goal to %d per %s and draft them", per, cadence),
			Description: fmt.Sprintf("Every %s plans %d from then on, since the goal is a rate.", cadence, per),
			Tool:        "setCampaignGoal", Args: SetCampaignGoalInput{PerPeriod: per, Draft: &in}})
	}
	opts[0].Recommended, opts[0].Why = true, overGoalWhy[opts[0].ID]
	return assistantkit.ConflictedCard(reason, card, opts...)
}

// overGoalWhy is why each way past the goal that can lead is recommended.
var overGoalWhy = map[string]string{
	"fill":    "It reaches the goal exactly, without going over.",
	"replace": "The campaign stays at its goal, and the new posts take the weakest drafts' place.",
	"ideas":   "Ideas don't count toward the goal, so nothing takes the campaign past it.",
}

// ideasOf is a draftPost call as a generatePosts one: the same platforms,
// count, type and date, saved as ideas.
func ideasOf(in DraftPostInput) GeneratePostsInput {
	return GeneratePostsInput{Platforms: in.Platforms, Count: in.Count, PostType: in.PostType, WindowStart: in.PublishDate, WindowEnd: in.PublishDate}
}

// raisedGoal is the fewest posts per period whose target over the campaign
// fits want posts; 0 when the campaign has no goal or dates to scale, or it
// would take more than maxGoalPerPeriod.
func raisedGoal(c *models.Campaign, want int) int {
	if c.EstimatedPostCount == nil {
		return 0
	}
	loc, err := settings.ResolveTimezone(c.Timezone)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	cadence := cmp.Or(c.GoalCadence, campaigngoal.DefaultCadence)
	for per := *c.EstimatedPostCount + 1; per <= maxGoalPerPeriod; per++ {
		target := campaigngoal.EffectiveCount(&per, cadence, c.StartDate, c.EndDate, loc)
		if target == 0 {
			return 0
		}
		if target >= want {
			return per
		}
	}
	return 0
}

// weakestDrafts are up to n of the campaign's plain drafts with a quality
// score, lowest first; none when the drafts or scores could not be read.
func weakestDrafts(ctx context.Context, n int) []models.Post {
	st := getRequestState(ctx)
	if st.deletePost == nil || st.repos.Evaluations == nil || st.repos.Posts == nil {
		return nil
	}
	posts, err := st.repos.Posts.ListRowsByCampaign(ctx, st.campaign.ID, "id", "title", "status")
	if err != nil {
		return nil
	}
	drafts := slices.DeleteFunc(posts, func(p models.Post) bool { return p.Status != models.PostStatusDraft })
	ids := make([]string, len(drafts))
	for i, p := range drafts {
		ids[i] = p.ID
	}
	scores, err := st.repos.Evaluations.OverallByPost(ctx, ids)
	if err != nil {
		return nil
	}
	drafts = slices.DeleteFunc(drafts, func(p models.Post) bool { _, ok := scores[p.ID]; return !ok })
	slices.SortStableFunc(drafts, func(a, b models.Post) int { return cmp.Compare(scores[a.ID], scores[b.ID]) })
	return drafts[:min(n, len(drafts))]
}

// SetCampaignGoalInput sets the campaign's posts per goal period, and may
// draft posts once it is set.
type SetCampaignGoalInput struct {
	PerPeriod int `json:"perPeriod" jsonschema:"description=Posts per goal period (the campaign's week or month)"`
	// Draft is the draftPost call the raise makes room for.
	Draft *DraftPostInput `json:"draft,omitempty" jsonschema:"description=Leave empty: set by the server when a raise makes room for posts to draft."`
}

// GoalResult is the campaign's goal after setCampaignGoal.
type GoalResult struct {
	PerPeriod int    `json:"perPeriod"`
	Cadence   string `json:"cadence"`
	// Target is the posts the whole campaign now aims for.
	Target int    `json:"target"`
	UndoID string `json:"undoId,omitempty"`
}

// SetCampaignGoalOutput is the new goal, and the drafting it made room for.
type SetCampaignGoalOutput struct {
	Goal    GoalResult       `json:"goal"`
	Drafted *DraftPostOutput `json:"drafted,omitempty"`
}

// setCampaignGoalGate always waits for a yes: the goal is what every later
// request is measured against.
var setCampaignGoalGate = assistantkit.Gate[SetCampaignGoalInput]{
	Precheck: func(ctx context.Context, in SetCampaignGoalInput) error {
		if in.PerPeriod <= 0 || in.PerPeriod > maxGoalPerPeriod {
			return assistantkit.Refused(fmt.Sprintf("Set between 1 and %d posts per period.", maxGoalPerPeriod))
		}
		return nil
	},
	Describe: func(ctx context.Context, in SetCampaignGoalInput) string {
		cadence := cmp.Or(getRequestState(ctx).campaign.GoalCadence, campaigngoal.DefaultCadence)
		out := fmt.Sprintf("Set the campaign's goal to %d posts per %s", in.PerPeriod, cadence)
		if in.Draft != nil {
			out += fmt.Sprintf(", then draft %d %s", draftTotal(ctx, *in.Draft), plural(draftTotal(ctx, *in.Draft), "post", "posts"))
		}
		return out
	},
}

func toolSetCampaignGoalFunc(ctx *ai.ToolContext, in SetCampaignGoalInput) (assistantkit.Outcome[SetCampaignGoalOutput], error) {
	return toolSetCampaignGoal(ctx, in)
}

func toolReplaceDraftsFunc(ctx *ai.ToolContext, in ReplaceDraftsInput) (assistantkit.Outcome[ReplaceDraftsOutput], error) {
	return toolReplaceDrafts(ctx, in)
}

// toolSetCampaignGoal sets the posts per goal period, logs the change so it
// can be undone, and drafts what the raise made room for.
func toolSetCampaignGoal(ctx context.Context, in SetCampaignGoalInput) (assistantkit.Outcome[SetCampaignGoalOutput], error) {
	st := getRequestState(ctx)
	c := st.campaign
	before := assistantundo.CampaignGoal(c)
	saved := &models.Campaign{ID: c.ID, EstimatedPostCount: new(in.PerPeriod)}
	if err := st.repos.Campaigns.UpdateColumns(ctx, nil, saved, "estimated_post_count"); err != nil {
		return assistantkit.Outcome[SetCampaignGoalOutput]{}, fmt.Errorf("update campaign goal: %w", err)
	}
	st.mu.Lock()
	c.EstimatedPostCount = new(in.PerPeriod)
	st.mu.Unlock()
	loc, err := settings.ResolveTimezone(c.Timezone)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	cadence := cmp.Or(c.GoalCadence, campaigngoal.DefaultCadence)
	target := campaigngoal.EffectiveCount(c.EstimatedPostCount, cadence, c.StartDate, c.EndDate, loc)
	// Later calls this turn measure against the new goal.
	if g := st.snapshot.Goal; g != nil {
		raised := *g
		raised.PerPeriod, raised.Target, raised.Room = in.PerPeriod, target, max(target-g.Planned, 0)
		st.snapshot.Goal = &raised
	}
	r := &GoalResult{PerPeriod: in.PerPeriod, Cadence: cadence, Target: target,
		UndoID: st.logChange(ctx, models.AssistantActionCampaignGoal, before, assistantundo.CampaignGoal(c))}
	st.goalResult = r
	st.recordWrite(actionGoalUpdated)
	out := SetCampaignGoalOutput{Goal: *r}
	if in.Draft != nil {
		drafted, err := toolDraftPost(ctx, *in.Draft)
		if err != nil {
			return assistantkit.Outcome[SetCampaignGoalOutput]{}, err
		}
		if !drafted.Succeeded() {
			return assistantkit.Refuse[SetCampaignGoalOutput]("The goal is raised, but drafting didn't run: "+drafted.Reason, drafted.Alternatives...), nil
		}
		out.Drafted = drafted.Result
	}
	return assistantkit.OK(out), nil
}

// ReplaceDraftsInput drafts posts and deletes as many of the named drafts.
type ReplaceDraftsInput struct {
	Delete []string       `json:"delete" jsonschema:"description=Leave to the server: the drafts the new ones replace"`
	Draft  DraftPostInput `json:"draft"`
}

// ReplaceDraftsOutput is what was drafted, and the drafts deleted for it.
// Drafted is always an object; its posts are the new drafts.
type ReplaceDraftsOutput struct {
	Drafted DraftPostOutput `json:"drafted"`
	Deleted int             `json:"deleted"`
	// Removed are the drafts deleted, weakest first, with their scores.
	Removed []PostRef `json:"removed,omitempty"`
	// Kept are the drafts left: changed since the offer, or more than the
	// posts written.
	Kept []string `json:"kept,omitempty"`
	// Period is the current goal period after the swap, e.g. "April stays
	// at 20"; nil without a goal, outside its dates, or when the posts
	// could not be read again.
	Period *state.Period `json:"period,omitempty"`
}

// replaceDraftsGate always waits for a yes: it deletes posts for good.
var replaceDraftsGate = assistantkit.Gate[ReplaceDraftsInput]{
	Precheck: func(ctx context.Context, in ReplaceDraftsInput) error {
		if len(in.Delete) == 0 {
			return assistantkit.Refused("Name the drafts to replace.")
		}
		if getRequestState(ctx).deletePost == nil {
			return assistantkit.Refused("Drafts can't be deleted from here.")
		}
		return nil
	},
	Describe: func(ctx context.Context, in ReplaceDraftsInput) string {
		titles := draftTitles(ctx, in.Delete)
		return fmt.Sprintf("Draft %d new %s, then delete %d weaker %s for good: %s",
			draftTotal(ctx, in.Draft), plural(draftTotal(ctx, in.Draft), "post", "posts"), len(in.Delete), plural(len(in.Delete), "draft", "drafts"), strings.Join(titles, "; "))
	},
}

// draftTitles names the drafts, by title or id.
func draftTitles(ctx context.Context, ids []string) []string {
	st := getRequestState(ctx)
	out := slices.Clone(ids)
	if st.repos.Posts == nil {
		return out
	}
	posts, err := st.repos.Posts.ListRowsByCampaign(ctx, st.campaign.ID, "id", "title")
	if err != nil {
		return out
	}
	for i, id := range out {
		if j := slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == id }); j >= 0 && posts[j].Title != "" {
			out[i] = posts[j].Title
		}
	}
	return out
}

// toolReplaceDrafts drafts the new posts first, then deletes as many of the
// named drafts as were written, each only while it is still a draft of this
// campaign: a draft edited into another status since stays.
func toolReplaceDrafts(ctx context.Context, in ReplaceDraftsInput) (assistantkit.Outcome[ReplaceDraftsOutput], error) {
	st := getRequestState(ctx)
	drafted, err := toolDraftPost(ctx, in.Draft)
	if err != nil {
		return assistantkit.Outcome[ReplaceDraftsOutput]{}, err
	}
	if !drafted.Succeeded() || drafted.Result == nil {
		return assistantkit.Refuse[ReplaceDraftsOutput]("Nothing was drafted, so no draft was deleted: "+drafted.Reason, drafted.Alternatives...), nil
	}
	out := ReplaceDraftsOutput{Drafted: *drafted.Result}
	actor, _ := logging.UserIDFrom(ctx)
	scores := draftScores(ctx, in.Delete)
	for i, id := range in.Delete {
		if out.Deleted >= drafted.Result.PostCount {
			out.Kept = append(out.Kept, in.Delete[i:]...)
			break
		}
		p, err := st.repos.Posts.GetByID(ctx, id)
		if err != nil || p.CampaignID != st.campaign.ID || p.Status != models.PostStatusDraft {
			out.Kept = append(out.Kept, id)
			continue
		}
		deleted, err := st.deletePost(ctx, p, actor)
		if err != nil {
			slog.WarnContext(ctx, "delete replaced draft failed", logging.AttrComponent, logComponent, "post_id", id, logging.AttrError, err)
			out.Kept = append(out.Kept, id)
			continue
		}
		if deleted {
			out.Deleted++
			ref := PostRef{ID: id, Title: titleOf(*p), Platform: platformName(st.campaign, p.PlatformID)}
			if s, ok := scores[id]; ok {
				ref.FitPct = new(int(math.Round(s)))
			}
			out.Removed = append(out.Removed, ref)
			assistantkit.Touch(ctx, "post", id)
		}
	}
	out.Period = currentPeriod(ctx)
	return assistantkit.OK(out), nil
}

// draftScores are the posts' overall quality scores; empty when they could
// not be read.
func draftScores(ctx context.Context, ids []string) map[string]float64 {
	st := getRequestState(ctx)
	if st.repos.Evaluations == nil {
		return nil
	}
	scores, err := st.repos.Evaluations.OverallByPost(ctx, ids)
	if err != nil {
		return nil
	}
	return scores
}

// currentPeriod reads the campaign's posts again for the goal period today
// falls in; nil without one, or when they could not be read.
func currentPeriod(ctx context.Context) *state.Period {
	st := getRequestState(ctx)
	if st.repos.Posts == nil {
		return nil
	}
	posts, err := st.repos.Posts.ListRowsByCampaign(ctx, st.campaign.ID, snapshotColumns...)
	if err != nil {
		return nil
	}
	p := calendarcheck.CurrentPeriod(st.campaign, posts, time.Now().UTC())
	if p == nil {
		return nil
	}
	return &state.Period{Label: p.Label, Target: p.Target, Planned: p.Planned, Left: max(p.Target-p.Planned, 0)}
}
