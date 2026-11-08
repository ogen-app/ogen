package campaign_assistant

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
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

// Drafted posts do fill the goal: past it, the user chooses between filling
// it exactly and adding them all anyway.
func TestOverGoal_OffersAChoice(t *testing.T) {
	var generated int
	st := budgetState(&memProposals{}, &generated, 2)
	ctx := st.bind(t.Context())
	args := map[string]any{"count": 10}
	err := overGoal(ctx, "draftPost", 10, args, func(n int) any { return map[string]any{"count": n} })
	ce, ok := errors.AsType[*assistantkit.ConflictError](err)
	if !ok || len(ce.Options) != 2 || ce.Options[0].ID != "fill" || ce.Options[1].ID != "anyway" {
		t.Fatalf("err = %v", err)
	}
	if fill := ce.Options[0].Args.(map[string]any); fill["count"] != 2 {
		t.Fatalf("fill = %+v", fill)
	}

	// No room: only "anyway". Within the goal: no conflict.
	st = budgetState(&memProposals{}, &generated, 0)
	if ce, ok := errors.AsType[*assistantkit.ConflictError](overGoal(st.bind(t.Context()), "draftPost", 1, args, nil)); !ok || len(ce.Options) != 1 || ce.Options[0].ID != "anyway" {
		t.Fatalf("no room = %+v", ce)
	}
	st = budgetState(&memProposals{}, &generated, 5)
	if err := overGoal(st.bind(t.Context()), "draftPost", 2, args, nil); err != nil {
		t.Fatalf("within the goal: %v", err)
	}
}

// When the posts could not be read the room is unknown: even one post waits
// for a yes, and nothing offers to fill the goal.
func TestOverGoal_UnknownRoomAsks(t *testing.T) {
	var generated int
	st := budgetState(&memProposals{}, &generated, 5)
	st.snapshot = st.snapshot.WithoutPosts()
	err := overGoal(st.bind(t.Context()), "draftPost", 1, map[string]any{"count": 1}, func(n int) any { return map[string]any{"count": n} })
	ce, ok := errors.AsType[*assistantkit.ConflictError](err)
	if !ok || len(ce.Options) != 1 || ce.Options[0].ID != "anyway" {
		t.Fatalf("err = %v", err)
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
	if !ok || len(ce.Options) != 2 || ce.Options[0].ID != "fill" {
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
