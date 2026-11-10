package campaign_assistant

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// budgetState is a turn whose campaign has room for room more posts.
func budgetState(store *memProposals, generated *int, room int) *requestState {
	st := proposalState(store, generated)
	st.snapshot = state.Snapshot{Goal: &state.Goal{Target: 10, Planned: 10 - room, Room: room}}
	return st
}

// Planning makes ideas, which fill no goal: with no room left a small plan
// still runs, and a big one waits for a yes only because of its size.
func TestGeneratePosts_IdeasFillNoGoal(t *testing.T) {
	store := &memProposals{}
	var generated int
	out := runTool(t, budgetState(store, &generated, 0), "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 1})
	if out.Status != assistantkit.StatusOK || generated != 1 {
		t.Fatalf("no room, 1 idea: outcome = %+v, generated = %d", out, generated)
	}

	out = runTool(t, budgetState(store, &generated, 2), "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 10})
	if out.Status != assistantkit.StatusProposed || len(out.Options) != 0 || generated != 1 {
		t.Fatalf("room for 2, 10 ideas: outcome = %+v, generated = %d; want a plain proposal, no goal choice", out, generated)
	}
	if out.Summary != "Plan 10 ideas for Threads" {
		t.Fatalf("summary = %q", out.Summary)
	}
}

func TestGeneratePosts_WithinGoalRuns(t *testing.T) {
	store := &memProposals{}
	var generated int
	out := runTool(t, budgetState(store, &generated, 5), "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
	if out.Status != assistantkit.StatusOK || generated != 1 || len(store.rows) != 0 {
		t.Fatalf("outcome = %+v, generated = %d, proposals = %d", out, generated, len(store.rows))
	}
}

// optionIDs lists a conflict's options, the recommended one marked with *.
func optionIDs(ce *assistantkit.ConflictError) []string {
	out := make([]string, len(ce.Options))
	for i, o := range ce.Options {
		out[i] = o.ID
		if o.Recommended {
			out[i] += "*"
		}
	}
	return out
}

// goalState is budgetState with a campaign goal of 2 posts a week to raise.
func goalState(room int) *requestState {
	var generated int
	st := budgetState(&memProposals{}, &generated, room)
	st.campaign.EstimatedPostCount, st.campaign.GoalCadence = new(2), "week"
	return st
}

// Drafted posts fill the goal: past it, the user picks between filling it
// exactly, saving them as ideas, and raising the goal; filling it is
// recommended.
func TestDraftOverGoal_OffersTheWaysForward(t *testing.T) {
	ctx := goalState(2).bind(t.Context())
	err := draftOverGoal(ctx, DraftPostInput{Platforms: []string{"Threads"}, Count: 10})
	ce, ok := errors.AsType[*assistantkit.ConflictError](err)
	if !ok || !slices.Equal(optionIDs(ce), []string{"fill*", "ideas", "raise"}) {
		t.Fatalf("err = %v", err)
	}
	if fill := ce.Options[0].Args.(DraftPostInput); fill.Count != 2 {
		t.Fatalf("fill = %+v", fill)
	}
	if ideas := ce.Options[1].Args.(GeneratePostsInput); ideas.Count != 10 || ce.Options[1].Tool != "generatePosts" {
		t.Fatalf("ideas = %+v", ideas)
	}
	raise := ce.Options[2].Args.(SetCampaignGoalInput)
	if raise.PerPeriod <= 2 || raise.Draft == nil || raise.Draft.Count != 10 || ce.Options[2].Tool != "setCampaignGoal" {
		t.Fatalf("raise = %+v", raise)
	}
	// Within the goal: no conflict.
	if err := draftOverGoal(goalState(5).bind(t.Context()), DraftPostInput{Platforms: []string{"Threads"}, Count: 2}); err != nil {
		t.Fatalf("within the goal: %v", err)
	}
}

// scoredDrafts are the campaign's drafts with a quality score each.
type scoredDrafts struct {
	repository.PostEvaluationRepository
	scores map[string]float64
}

func (s scoredDrafts) OverallByPost(_ context.Context, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, id := range ids {
		if v, ok := s.scores[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// With no room left and enough scored drafts, replacing the weakest leads;
// without them, saving as ideas does.
func TestDraftOverGoal_ReplaceTheWeakest(t *testing.T) {
	st := goalState(0)
	st.deletePost = func(context.Context, *models.Post, string) (bool, error) { return true, nil }
	st.repos.Posts = platformPosts{posts: []models.Post{
		{ID: "d1", Status: models.PostStatusDraft}, {ID: "d2", Status: models.PostStatusDraft},
		{ID: "d3", Status: models.PostStatusDraft}, {ID: "r1", Status: models.PostStatusReadyForPublish},
	}}
	st.repos.Evaluations = scoredDrafts{scores: map[string]float64{"d1": 70, "d2": 40, "r1": 10}}
	ctx := st.bind(t.Context())

	ce, _ := errors.AsType[*assistantkit.ConflictError](draftOverGoal(ctx, DraftPostInput{Platforms: []string{"Threads"}, Count: 2}))
	if ce == nil || !slices.Equal(optionIDs(ce), []string{"replace*", "ideas", "raise"}) {
		t.Fatalf("conflict = %+v", ce)
	}
	// The weakest first, drafts only: the ready post scores lower but stays.
	if in := ce.Options[0].Args.(ReplaceDraftsInput); !slices.Equal(in.Delete, []string{"d2", "d1"}) || ce.Options[0].Tool != "replaceWeakestDrafts" {
		t.Fatalf("replace = %+v", in)
	}
	// Three asked, two scored drafts: no replace.
	ce, _ = errors.AsType[*assistantkit.ConflictError](draftOverGoal(ctx, DraftPostInput{Platforms: []string{"Threads"}, Count: 3}))
	if ce == nil || !slices.Equal(optionIDs(ce), []string{"ideas*", "raise"}) {
		t.Fatalf("conflict = %+v", ce)
	}
}

// When the posts could not be read the room is unknown: saving as ideas is
// recommended, and drafting without the check is the other way.
func TestDraftOverGoal_UnknownRoom(t *testing.T) {
	st := goalState(5)
	st.snapshot = st.snapshot.WithoutPosts()
	ce, _ := errors.AsType[*assistantkit.ConflictError](draftOverGoal(st.bind(t.Context()), DraftPostInput{Platforms: []string{"Threads"}, Count: 1}))
	if ce == nil || !slices.Equal(optionIDs(ce), []string{"ideas*", "anyway"}) {
		t.Fatalf("conflict = %+v", ce)
	}
}

// A platform named twice is drafted once, so it counts once against the goal.
func TestDraftPostGate_CountsDistinctPlatforms(t *testing.T) {
	var generated int
	ctx := budgetState(&memProposals{}, &generated, 1).bind(t.Context())
	in := DraftPostInput{Platforms: []string{"Threads", "Threads"}, Count: 1}
	if err := draftPostGate.Precheck(ctx, in); err != nil {
		t.Fatalf("precheck = %v, want one post to fit the room of 1", err)
	}
	if draftPostGate.Needs(ctx, DraftPostInput{Platforms: []string{"Threads", "threads"}, Count: 3}) {
		t.Fatal("3 posts on one platform are within the threshold")
	}
	// Over the goal, one distinct platform can still fill it exactly.
	in.Count = 2
	ce, ok := errors.AsType[*assistantkit.ConflictError](draftPostGate.Precheck(ctx, in))
	if !ok || !slices.Equal(optionIDs(ce), []string{"fill*", "ideas"}) {
		t.Fatalf("precheck conflict = %+v", ce)
	}
}

// The gates compare the threshold with what the tool would create, which is
// capped per call.
func TestPostGates_ThresholdUsesCappedCount(t *testing.T) {
	st := &requestState{campaign: timelineCampaign(), proposalThreshold: 5, maxDraftPosts: 5, maxGeneratePosts: 5}
	ctx := st.bind(t.Context())
	if draftPostGate.Needs(ctx, DraftPostInput{Platforms: []string{"LinkedIn", "Threads"}, Count: 10}) {
		t.Fatal("draftPost: 10 on two platforms creates 5, within the threshold")
	}
	if generatePostsGate.Needs(ctx, GeneratePostsInput{Platforms: []string{"Threads"}, Count: 20}) {
		t.Fatal("generatePosts: 20 creates 5, within the threshold")
	}
	st.proposalThreshold = 4
	if !draftPostGate.Needs(ctx, DraftPostInput{Platforms: []string{"LinkedIn", "Threads"}, Count: 10}) {
		t.Fatal("draftPost: 5 posts are above a threshold of 4")
	}
}

// postsByID reads the campaign's posts one by one too.
type postsByID struct{ platformPosts }

func (p postsByID) GetByID(_ context.Context, id string) (*models.Post, error) {
	for i := range p.posts {
		if p.posts[i].ID == id {
			cp := p.posts[i]
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}

// Raising the goal saves it, undoably, measures the rest of the turn against
// it, and drafts what it made room for.
func TestSetCampaignGoal_RaisesAndDrafts(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.AssistantMessage{modelMsg(`{"action":"answered","explanation":"the research"}`)}, &calls)
	st.campaign.ID, st.campaign.EstimatedPostCount, st.campaign.GoalCadence = "c1", new(2), "week"
	stored := *st.campaign
	campaigns := &storedCampaign{c: &stored}
	st.repos.Campaigns = campaigns
	st.snapshot = state.Snapshot{Goal: &state.Goal{Target: 9, Planned: 9, Room: 0}}

	out, err := toolSetCampaignGoal(withRequestState(t.Context(), st), SetCampaignGoalInput{PerPeriod: 4, Draft: &DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1}})
	if err != nil || !out.Succeeded() || out.Result.Goal.PerPeriod != 4 || out.Result.Drafted == nil || out.Result.Drafted.PostCount != 1 {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if *st.campaign.EstimatedPostCount != 4 || !slices.Contains(campaigns.columns[0], "estimated_post_count") || st.goalResult == nil {
		t.Fatalf("campaign goal = %d, columns = %v", *st.campaign.EstimatedPostCount, campaigns.columns)
	}
	if g := st.snapshot.Goal; g.Target <= 9 || g.Room != g.Target-9 {
		t.Fatalf("goal after = %+v", g)
	}
	if st.writes[0] != actionGoalUpdated {
		t.Fatalf("writes = %v", st.writes)
	}
}

// Replacing drafts writes first, then deletes as many of the named drafts as
// were written, and only ones still drafts of the campaign.
func TestReplaceWeakestDrafts(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.AssistantMessage{modelMsg(`{"action":"answered","explanation":"the research"}`)}, &calls)
	st.campaign.ID = "c1"
	st.repos.Posts = postsByID{platformPosts{posts: []models.Post{
		{ID: "d1", CampaignID: "c1", Status: models.PostStatusReadyForPublish},
		{ID: "d2", CampaignID: "c1", Status: models.PostStatusDraft},
		{ID: "d3", CampaignID: "c1", Status: models.PostStatusDraft},
	}}}
	var deleted []string
	st.deletePost = func(_ context.Context, p *models.Post, _ string) (bool, error) {
		deleted = append(deleted, p.ID)
		return true, nil
	}
	out, err := toolReplaceDrafts(withRequestState(t.Context(), st), ReplaceDraftsInput{Delete: []string{"d1", "d2", "d3"},
		Draft: DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1}})
	if err != nil || !out.Succeeded() || out.Result.Deleted != 1 || !slices.Equal(deleted, []string{"d2"}) || !slices.Equal(out.Result.Kept, []string{"d1", "d3"}) {
		t.Fatalf("out = %+v, err = %v, deleted = %v", out, err, deleted)
	}
}
