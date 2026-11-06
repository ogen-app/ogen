package campaign_assistant

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// budgetState is a turn whose campaign has room for room more posts.
func budgetState(store *memProposals, generated *int, room int) *requestState {
	st := proposalState(store, generated)
	st.snapshot = state.Snapshot{Goal: &state.Goal{Target: 10, Planned: 10 - room, Room: room}}
	return st
}

// The Daria case: the goal has room for 2, the request is for 10. Nothing
// runs; the user gets a choice, and picking "fill" adds exactly 2.
func TestGeneratePosts_OverGoalOffersAChoice(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := budgetState(store, &generated, 2)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 10})
	if out.Status != assistantkit.StatusConflict || out.ProposalID == "" || generated != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, generated)
	}
	if len(out.Options) != 2 || out.Options[0].ID != "fill" || out.Options[1].ID != "anyway" {
		t.Fatalf("options = %+v", out.Options)
	}
	p := store.rows[0]
	if fill, _ := p.Option("fill"); fill.Tool != "generatePosts" || fill.Args["count"] != float64(2) {
		t.Fatalf("stored fill option = %+v", fill)
	}
	if len(st.proposed) != 1 || len(st.proposed[0].Options) != 2 {
		t.Fatalf("card payload = %+v", st.proposed)
	}

	st.registry = newTestTools(t).reg
	// A plain yes is not enough when there is a choice.
	if out := runTool(t, st, "confirmProposal", map[string]any{"proposalId": p.ID}); out.Status != assistantkit.StatusRefused || generated != 0 {
		t.Fatalf("confirm without an option = %+v, generated = %d", out, generated)
	}
	out = runTool(t, st, "confirmProposal", map[string]any{"proposalId": p.ID, "optionId": "fill"})
	if out.Status != assistantkit.StatusOK || generated != 1 || p.Status != models.ProposalConfirmed || p.Chosen != "fill" {
		t.Fatalf("confirm fill = %+v, generated = %d, proposal = %+v", out, generated, p)
	}
}

// With no room left, the only way forward is past the goal, and it still
// waits for a yes.
func TestGeneratePosts_NoRoomOffersOnlyAnyway(t *testing.T) {
	store := &memProposals{}
	var generated int
	out := runTool(t, budgetState(store, &generated, 0), "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 1})
	if out.Status != assistantkit.StatusConflict || len(out.Options) != 1 || out.Options[0].ID != "anyway" || generated != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, generated)
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
