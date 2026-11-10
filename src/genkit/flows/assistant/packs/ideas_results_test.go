package packs

import (
	"context"
	"slices"
	"testing"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// A turn on the ideas list that triages without naming ideas works on the
// open one and the selected ones, and completes with both outputs under
// results.ideas, the single verdict's undo id with them.
func TestIdeasEnd_ThroughATurn(t *testing.T) {
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	g := genkit.Init(ctx)
	d, repo, _ := ideaDeps()
	repo.byID["i3"] = &models.Idea{ID: "i3", Title: "Onboarding tips"}
	d.Compose = compose.New(nil, repo, nil, nil, nil, nil)
	a, err := assistant.Init(g, assistant.FlowConfig{Provider: llm.NewProvider(), MaxOutputTokens: 1024},
		assistant.Repos{Messages: turnMessages{}, Proposals: &storedProposals{}}, ideasPack(d))
	if err != nil {
		t.Fatal(err)
	}
	m := fakemodel.Define(g, "orchestrator",
		fakemodel.Tools(fakemodel.Call{Name: "setIdeaVerdict", Input: map[string]any{"verdict": "yes"}},
			fakemodel.Call{Name: "setIdeaVerdicts", Input: map[string]any{"verdict": "no"}}),
		fakemodel.Answer(`{"explanation":"Done.","action":"answered"}`))
	fakemodel.Route(ctx, m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})

	view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "ideas", Scope: workspace,
		Focus:     &viewcontext.Ref{Kind: viewcontext.KindIdea, ID: "i1"},
		Selection: &viewcontext.Selection{Kind: viewcontext.KindIdea, IDs: []string{"i2", "i3"}}},
		Focus: &viewresolve.Item{Kind: viewcontext.KindIdea, ID: "i1", Title: "Pricing myths"}, Summary: "On the ideas list"}
	resp, err := a.Run(ctx, assistant.Request{Instruction: "Yes to this one, no to the selected.", View: view}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.Results["ideas"].(map[string]any)
	if !ok {
		t.Fatalf("results = %#v", resp.Results)
	}
	one, ok := got["setIdeaVerdict"].(*SetIdeaVerdictOutput)
	if !ok || one.Idea.ID != "i1" || one.Idea.Verdict != "yes" || one.UndoID == "" {
		t.Fatalf("setIdeaVerdict = %#v", got["setIdeaVerdict"])
	}
	bulk, ok := got["setIdeaVerdicts"].(*BulkIdeasOutput)
	if !ok || bulk.Done != 2 || *repo.byID["i2"].Verdict != models.IdeaVerdictNo || *repo.byID["i3"].Verdict != models.IdeaVerdictNo {
		t.Fatalf("setIdeaVerdicts = %#v", got["setIdeaVerdicts"])
	}
}

// Without ids a call means the selection, else the open idea; a single-idea
// call the open idea, else a lone selection.
func TestIdeasOnScreen(t *testing.T) {
	view := func(focus string, sel ...string) context.Context {
		v := viewcontext.ViewContext{Version: viewcontext.Version, Screen: "ideas", Scope: workspace}
		if focus != "" {
			v.Focus = &viewcontext.Ref{Kind: viewcontext.KindIdea, ID: focus}
		}
		if len(sel) > 0 {
			v.Selection = &viewcontext.Selection{Kind: viewcontext.KindIdea, IDs: sel}
		}
		ctx, _ := assistant.NewViewTurnContext(t.Context(), v, nil)
		return ctx
	}
	cases := []struct {
		ctx       context.Context
		ids       []string
		wantMany  []string
		wantOne   string
		passedOne string
	}{
		{view("i1", "i2", "i3"), nil, []string{"i2", "i3"}, "i1", ""},
		{view("", "i2"), nil, []string{"i2"}, "i2", ""},
		{view("", "i2", "i3"), nil, []string{"i2", "i3"}, "", ""},
		{view("i1"), []string{"i9"}, []string{"i9"}, "i9", "i9"},
		{view(""), nil, nil, "", ""},
	}
	for i, c := range cases {
		if got := ideasOnScreen(c.ctx, c.ids); !slices.Equal(got, c.wantMany) {
			t.Errorf("%d: ideasOnScreen = %v, want %v", i, got, c.wantMany)
		}
		if got := ideaOnScreen(c.ctx, c.passedOne); got != c.wantOne {
			t.Errorf("%d: ideaOnScreen = %q, want %q", i, got, c.wantOne)
		}
	}
}
