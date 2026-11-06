package campaign_assistant

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// sourcesState is a turn whose campaign has ready sources ready assets.
func sourcesState(store *memProposals, generated *int, ready int, used []content_plan.AssetRef) *requestState {
	st := proposalState(store, generated)
	st.snapshot = state.Snapshot{Sources: &state.Sources{Ready: ready}}
	st.generatePosts = func(_ context.Context, req content_plan.GeneratePostsRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		*generated++
		return &content_plan.ContentPlanResponse{Posts: make([]content_plan.DraftPost, req.Count), UsedAssets: used}, nil
	}
	return st
}

// No sources and no explicit ask: generation becomes a choice instead of a
// silent fallback to general knowledge.
func TestGeneratePosts_NoSourcesOffersExploratory(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := sourcesState(store, &generated, 0, nil)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
	if out.Status != assistantkit.StatusConflict || len(out.Options) != 1 || out.Options[0].ID != "exploratory" || generated != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, generated)
	}
	if o, _ := store.rows[0].Option("exploratory"); o.Args["exploratory"] != true {
		t.Fatalf("exploratory option args = %+v", o.Args)
	}

	// A plain yes takes the only way forward.
	st.registry = newTestTools(t).reg
	out = runTool(t, st, "confirmProposal", map[string]any{"proposalId": store.rows[0].ID})
	if out.Status != assistantkit.StatusOK || generated != 1 || store.rows[0].Chosen != "exploratory" {
		t.Fatalf("plain yes = %+v, generated = %d, chosen = %q", out, generated, store.rows[0].Chosen)
	}
	if g := st.generatedPostsResult.Grounding; g.Mode != assistantkit.Exploratory {
		t.Fatalf("grounding = %+v", g)
	}
}

// An explicit ask runs, and the result says it is not grounded.
func TestGeneratePosts_ExplicitExploratoryIsLabelled(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := sourcesState(store, &generated, 0, nil)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2, "exploratory": true})
	if out.Status != assistantkit.StatusOK || generated != 1 {
		t.Fatalf("outcome = %+v, generated = %d", out, generated)
	}
	if g := st.generatedPostsResult.Grounding; g.Mode != assistantkit.Exploratory || len(g.Sources) != 0 {
		t.Fatalf("grounding = %+v", g)
	}
}

// With sources, the result names the ones that informed the posts. A campaign
// that has sources but whose posts used none is still labelled exploratory.
func TestGeneratePosts_GroundingFromUsedAssets(t *testing.T) {
	cases := map[string]struct {
		used []content_plan.AssetRef
		mode assistantkit.GroundingMode
	}{
		"drew on a source": {[]content_plan.AssetRef{{ID: "a1", Title: "Hotel de Russie brochure"}}, assistantkit.Grounded},
		"used none":        {nil, assistantkit.Exploratory},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &memProposals{}
			var generated int
			st := sourcesState(store, &generated, 2, tc.used)
			out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
			if out.Status != assistantkit.StatusOK {
				t.Fatalf("outcome = %+v", out)
			}
			g := st.generatedPostsResult.Grounding
			if g.Mode != tc.mode || len(g.Sources) != len(tc.used) {
				t.Fatalf("grounding = %+v", g)
			}
		})
	}
}

// The content plan, always proposed, offers the exploratory choice when
// there are no sources.
func TestRunContentPlan_NoSourcesOffersExploratory(t *testing.T) {
	store := &memProposals{}
	var generated int
	out := runTool(t, sourcesState(store, &generated, 0, nil), "runContentPlan", map[string]any{})
	if out.Status != assistantkit.StatusConflict || out.Options[0].ID != "exploratory" || generated != 0 {
		t.Fatalf("outcome = %+v", out)
	}
}
