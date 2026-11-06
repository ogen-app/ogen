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
