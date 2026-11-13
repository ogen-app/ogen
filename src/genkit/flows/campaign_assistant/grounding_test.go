package campaign_assistant

import (
	"context"
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
	// offered is the engine's UsedAssets; cited is every idea's AssetRefs.
	offered []content_plan.AssetRef
	cited   []string
}

func (g *generation) run(ctx context.Context, n int) *content_plan.ContentPlanResponse {
	g.count++
	g.excluded = content_plan.AssetsExcluded(ctx)
	ideas := make([]content_plan.DraftPost, n)
	for i := range ideas {
		ideas[i].AssetRefs = g.cited
	}
	return &content_plan.ContentPlanResponse{Ideas: ideas, UsedAssets: g.offered}
}

// sourcesState is a turn whose campaign has ready sources ready assets.
func sourcesState(store *memProposals, gen *generation, ready int) *requestState {
	var unused int
	st := proposalState(store, &unused)
	st.snapshot = state.Snapshot{Sources: &state.Sources{Ready: ready}}
	st.generatePosts = func(ctx context.Context, req content_plan.GeneratePostsRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		return gen.run(ctx, req.Count), nil
	}
	st.contentPlan = func(ctx context.Context, _ content_plan.ContentPlanRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		return gen.run(ctx, 2), nil
	}
	return st
}

// confirm runs the stored call, or the named option's call, past the gate.
func confirm(t *testing.T, st *requestState, store *memProposals, option string) {
	t.Helper()
	p := store.rows[0]
	tool, args := p.Tool, p.Args
	if option != "" {
		o, ok := p.Option(option)
		if !ok {
			t.Fatalf("no option %q", option)
		}
		tool, args = o.Tool, o.Args
	}
	raw, err := newTestTools(t).reg.RunConfirmed(st.bind(t.Context()), tool, map[string]any(args))
	if res, _ := raw.(map[string]any); err != nil || res["status"] != string(assistantkit.StatusOK) {
		t.Fatalf("confirming = %#v, %v", raw, err)
	}
}

// No sources and no explicit ask: generation becomes a choice instead of a
// silent fallback to general knowledge.
func TestGeneratePosts_NoSourcesOffersExploratory(t *testing.T) {
	store := &memProposals{}
	gen := &generation{}
	st := sourcesState(store, gen, 0)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
	if out.Status != assistantkit.StatusConflict || len(out.Options) != 3 || out.Options[0].ID != "add_document" || !out.Options[0].Recommended ||
		out.Options[1].ID != "import_page" || out.Options[1].Input == nil || out.Options[1].Input.Kind != assistantkit.OptionInputURL ||
		out.Options[2].ID != "exploratory" || gen.count != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, gen.count)
	}
	// The import's link is the value its card asks for.
	if o, _ := store.rows[0].Option("import_page"); o.Tool != "importPage" || o.Input == nil || o.Input.Arg != "url" {
		t.Fatalf("import option = %+v", o)
	}
	if o, _ := store.rows[0].Option("exploratory"); o.Args["exploratory"] != true {
		t.Fatalf("exploratory option args = %+v", o.Args)
	}

	// Taking the option runs its call past the gate, without assets.
	confirm(t, st, store, "exploratory")
	if gen.count != 1 || !gen.excluded {
		t.Fatalf("generated = %d, excluded = %v", gen.count, gen.excluded)
	}
	if g := st.generatedPostsResult.Grounding; g.Mode != assistantkit.Exploratory || g.Reason != assistantkit.ChoseWithout || g.Note == "" {
		t.Fatalf("grounding = %+v", g)
	}
}

// Adding a document opens the upload and generates nothing: outside the
// assistant's chat there is no screen to open, so it says where to add one.
func TestNoSources_AddDocumentOption(t *testing.T) {
	store := &memProposals{}
	gen := &generation{}
	st := sourcesState(store, gen, 0)
	runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 2})
	if o, _ := store.rows[0].Option("add_document"); o.Tool != "openAddSource" || o.Args["via"] != "upload" {
		t.Fatalf("add document option = %+v", o)
	}
	confirm(t, st, store, "add_document")
	if gen.count != 0 || st.generatedPostsResult != nil {
		t.Fatalf("adding a source generated %d", gen.count)
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

// With sources, the result names only the offered ones the ideas cite, and
// says why when it cites none.
func TestGeneratePosts_GroundingFromCitedAssets(t *testing.T) {
	brochure := content_plan.AssetRef{ID: "a1", Title: "Hotel de Russie brochure"}
	cases := map[string]struct {
		offered []content_plan.AssetRef
		cited   []string
		mode    assistantkit.GroundingMode
		sources int
		reason  assistantkit.UngroundedReason
	}{
		"cites an offered source": {[]content_plan.AssetRef{brochure, {ID: "a2", Title: "Menu"}}, []string{"a1"}, assistantkit.Grounded, 1, ""},
		"offered but not cited":   {[]content_plan.AssetRef{brochure}, nil, assistantkit.Exploratory, 0, assistantkit.NoneRelevant},
		"cites an unoffered id":   {[]content_plan.AssetRef{brochure}, []string{"zz"}, assistantkit.Exploratory, 0, assistantkit.NoneRelevant},
		"nothing offered":         {nil, nil, assistantkit.Exploratory, 0, assistantkit.NoSources},
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
			if g.Mode != tc.mode || len(g.Sources) != tc.sources || g.Reason != tc.reason || (g.Note == "") != (tc.reason == "") {
				t.Fatalf("grounding = %+v", g)
			}
		})
	}
}

// The content plan, always proposed, offers the exploratory choice when
// there are no sources; taking it runs without assets and is labelled
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
			if out.Status != assistantkit.StatusConflict || out.Options[0].ID != "add_document" || out.Options[2].ID != "exploratory" || gen.count != 0 {
				t.Fatalf("outcome = %+v", out)
			}

			confirm(t, st, store, "exploratory")
			if gen.count != 1 || !gen.excluded {
				t.Fatalf("generated = %d, excluded = %v", gen.count, gen.excluded)
			}
			if g := st.contentPlanResult.Grounding; g.Mode != assistantkit.Exploratory || len(g.Sources) != 0 {
				t.Fatalf("grounding = %+v", g)
			}
		})
	}
}

// With sources, a confirmed content plan uses them and is grounded by what
// its ideas cite.
func TestRunContentPlan_GroundedWithSources(t *testing.T) {
	store := &memProposals{}
	gen := &generation{offered: []content_plan.AssetRef{{ID: "a1", Title: "Brochure"}}, cited: []string{"a1"}}
	st := sourcesState(store, gen, 1)
	out := runTool(t, st, "runContentPlan", map[string]any{})
	if out.Status != assistantkit.StatusProposed {
		t.Fatalf("outcome = %+v", out)
	}
	confirm(t, st, store, "")
	if gen.excluded {
		t.Fatal("a grounded plan was told to leave assets out")
	}
	if g := st.contentPlanResult.Grounding; g.Mode != assistantkit.Grounded || len(g.Sources) != 1 {
		t.Fatalf("grounding = %+v", g)
	}
}
