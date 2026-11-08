package campaign_assistant

import (
	"context"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// generation is what the stubbed engine saw and returns.
type generation struct {
	count int
	// excluded records whether the last run was told to leave assets out.
	excluded bool
	// offered is the engine's UsedAssets; cited is every post's AssetRefs.
	offered []content_plan.AssetRef
	cited   []string
}

func (g *generation) run(ctx context.Context, n int) *content_plan.ContentPlanResponse {
	g.count++
	g.excluded = content_plan.AssetsExcluded(ctx)
	posts := make([]content_plan.DraftPost, n)
	for i := range posts {
		posts[i].AssetRefs = g.cited
	}
	return &content_plan.ContentPlanResponse{Posts: posts, UsedAssets: g.offered}
}

// sourcesState is a turn whose campaign has ready sources ready assets.
func sourcesState(store *memProposals, gen *generation, ready int) *requestState {
	var unused int
	st := proposalState(store, &unused)
	st.snapshot = state.Snapshot{Sources: &state.Sources{Ready: ready}}
	st.generatePosts = func(ctx context.Context, req content_plan.GeneratePostsRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		return gen.run(ctx, req.Count), nil
	}
	st.contentPlan = func(ctx context.Context, _ string, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		return gen.run(ctx, 2), nil
	}
	return st
}

// No sources and no explicit ask: generation becomes a choice instead of a
// silent fallback to general knowledge.
func TestGeneratePosts_NoSourcesOffersExploratory(t *testing.T) {
	store := &memProposals{}
	gen := &generation{}
	st := sourcesState(store, gen, 0)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
	if out.Status != assistantkit.StatusConflict || len(out.Options) != 1 || out.Options[0].ID != "exploratory" || gen.count != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, gen.count)
	}
	if o, _ := store.rows[0].Option("exploratory"); o.Args["exploratory"] != true {
		t.Fatalf("exploratory option args = %+v", o.Args)
	}

	// A plain yes takes the only way forward.
	st.registry = newTestTools(t).reg
	out = runTool(t, st, "confirmProposal", map[string]any{"proposalId": store.rows[0].ID})
	if out.Status != assistantkit.StatusOK || gen.count != 1 || store.rows[0].Chosen != "exploratory" {
		t.Fatalf("plain yes = %+v, generated = %d, chosen = %q", out, gen.count, store.rows[0].Chosen)
	}
	if g := st.generatedPostsResult.Grounding; g.Mode != assistantkit.Exploratory {
		t.Fatalf("grounding = %+v", g)
	}
}

// Past the goal with no sources, the goal choice is still offered, and every
// option goes ahead without sources.
func TestGeneratePosts_NoSourcesPastGoalKeepsGoalChoice(t *testing.T) {
	store := &memProposals{}
	gen := &generation{}
	st := sourcesState(store, gen, 0)
	st.snapshot.Goal = &state.Goal{Target: 10, Planned: 8, Room: 2}
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 5})
	if out.Status != assistantkit.StatusConflict || len(out.Options) != 2 || gen.count != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, gen.count)
	}
	if !strings.Contains(out.Summary, "no sources") || !strings.Contains(out.Summary, "goal") {
		t.Fatalf("summary = %q", out.Summary)
	}
	for _, id := range []string{"fill", "anyway"} {
		o, ok := store.rows[0].Option(id)
		if !ok || o.Args["exploratory"] != true || !strings.Contains(o.Title, "without sources") {
			t.Fatalf("option %s = %+v", id, o)
		}
	}

	st.registry = newTestTools(t).reg
	out = runTool(t, st, "confirmProposal", map[string]any{"proposalId": store.rows[0].ID, "optionId": "fill"})
	if out.Status != assistantkit.StatusOK || gen.count != 1 || st.generatedPostsResult.PostCount != 2 || !gen.excluded {
		t.Fatalf("fill = %+v, generated = %d, excluded = %v", out, gen.count, gen.excluded)
	}
}

// An explicit ask runs without the campaign's assets, even when it has ready
// ones, and the result says it is not grounded.
func TestGeneratePosts_ExplicitExploratoryLeavesAssetsOut(t *testing.T) {
	for name, ready := range map[string]int{"no sources": 0, "ready sources": 2} {
		t.Run(name, func(t *testing.T) {
			store := &memProposals{}
			gen := &generation{}
			st := sourcesState(store, gen, ready)
			out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2, "exploratory": true})
			if out.Status != assistantkit.StatusOK || gen.count != 1 || !gen.excluded {
				t.Fatalf("outcome = %+v, generated = %d, excluded = %v", out, gen.count, gen.excluded)
			}
			if g := st.generatedPostsResult.Grounding; g.Mode != assistantkit.Exploratory || len(g.Sources) != 0 {
				t.Fatalf("grounding = %+v", g)
			}
		})
	}
}

// With sources, the result names only the offered ones the posts cite.
func TestGeneratePosts_GroundingFromCitedAssets(t *testing.T) {
	brochure := content_plan.AssetRef{ID: "a1", Title: "Hotel de Russie brochure"}
	cases := map[string]struct {
		offered []content_plan.AssetRef
		cited   []string
		mode    assistantkit.GroundingMode
		sources int
	}{
		"cites an offered source": {[]content_plan.AssetRef{brochure, {ID: "a2", Title: "Menu"}}, []string{"a1"}, assistantkit.Grounded, 1},
		"offered but not cited":   {[]content_plan.AssetRef{brochure}, nil, assistantkit.Exploratory, 0},
		"cites an unoffered id":   {[]content_plan.AssetRef{brochure}, []string{"zz"}, assistantkit.Exploratory, 0},
		"nothing offered":         {nil, nil, assistantkit.Exploratory, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &memProposals{}
			gen := &generation{offered: tc.offered, cited: tc.cited}
			st := sourcesState(store, gen, 2)
			out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
			if out.Status != assistantkit.StatusOK || gen.excluded {
				t.Fatalf("outcome = %+v, excluded = %v", out, gen.excluded)
			}
			g := st.generatedPostsResult.Grounding
			if g.Mode != tc.mode || len(g.Sources) != tc.sources {
				t.Fatalf("grounding = %+v", g)
			}
		})
	}
}

// The content plan, always proposed, offers the exploratory choice when
// there are no sources; confirming it runs without assets and is labelled
// exploratory whatever the engine reports.
func TestRunContentPlan_NoSourcesOffersExploratory(t *testing.T) {
	cases := map[string]*generation{
		"engine reports no assets": {},
		"engine reports assets":    {offered: []content_plan.AssetRef{{ID: "a1", Title: "Brochure"}}, cited: []string{"a1"}},
	}
	for name, gen := range cases {
		t.Run(name, func(t *testing.T) {
			store := &memProposals{}
			st := sourcesState(store, gen, 0)
			out := runTool(t, st, "runContentPlan", map[string]any{})
			if out.Status != assistantkit.StatusConflict || out.Options[0].ID != "exploratory" || gen.count != 0 {
				t.Fatalf("outcome = %+v", out)
			}

			st.registry = newTestTools(t).reg
			out = runTool(t, st, "confirmProposal", map[string]any{"proposalId": store.rows[0].ID})
			if out.Status != assistantkit.StatusOK || gen.count != 1 || !gen.excluded {
				t.Fatalf("confirm = %+v, generated = %d, excluded = %v", out, gen.count, gen.excluded)
			}
			if g := st.contentPlanResult.Grounding; g.Mode != assistantkit.Exploratory || len(g.Sources) != 0 {
				t.Fatalf("grounding = %+v", g)
			}
		})
	}
}

// With sources, a confirmed content plan uses them and is grounded by what
// its posts cite.
func TestRunContentPlan_GroundedWithSources(t *testing.T) {
	store := &memProposals{}
	gen := &generation{offered: []content_plan.AssetRef{{ID: "a1", Title: "Brochure"}}, cited: []string{"a1"}}
	st := sourcesState(store, gen, 1)
	out := runTool(t, st, "runContentPlan", map[string]any{})
	if out.Status != assistantkit.StatusProposed {
		t.Fatalf("outcome = %+v", out)
	}
	st.registry = newTestTools(t).reg
	out = runTool(t, st, "confirmProposal", map[string]any{"proposalId": store.rows[0].ID})
	if out.Status != assistantkit.StatusOK || gen.excluded {
		t.Fatalf("confirm = %+v, excluded = %v", out, gen.excluded)
	}
	if g := st.contentPlanResult.Grounding; g.Mode != assistantkit.Grounded || len(g.Sources) != 1 {
		t.Fatalf("grounding = %+v", g)
	}
}
