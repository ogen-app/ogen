package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// importIn is importThing's input: the page to import.
type importIn struct {
	URL string `json:"url"`
}

// importerPack has one tool that records the pages it is asked to import.
func importerPack(imported *[]string) Pack {
	return Pack{
		Name: "importer", Summary: "imports", Scopes: []models.AssistantScopeKind{models.AssistantScopeWorkspace},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "importThing", "Imports a page.",
				assistantkit.Wrap(func(_ context.Context, in importIn) (string, error) {
					*imported = append(*imported, in.URL)
					return "imported", nil
				}))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "importer", Scopes: allScopes, Tier: assistantkit.TierRead, Guide: "Imports a page.", Tool: tool}}, nil
		},
	}
}

// An option that asks for a value stores what its argument is and shows the
// field on its card, without the argument; picked without the value it is
// refused and stays open, and with it runs with the value in that argument.
func TestProposals_OptionTakesInput(t *testing.T) {
	var imported []string
	store := &memProposals{}
	h := proposalsHarness(t, store, bulkPack(new(int)), importerPack(&imported))
	st := newTurnState(&viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "home",
		Scope: viewcontext.Scope{Kind: models.AssistantScopeWorkspace}}}, nil)
	input := &assistantkit.OptionInput{Kind: assistantkit.OptionInputURL, Label: "Page link", Arg: "url"}
	id, err := turnSink{st: st, repos: Repos{Proposals: store}}.Propose(t.Context(), assistantkit.Proposal{Tool: "bulkThing", Args: bulkIn{N: 9}, Summary: "No sources",
		Options: []assistantkit.Option{{ID: "import_page", Title: "Import a page", Input: input, Tool: "importThing", Args: struct{}{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if in := store.rows[0].Options[0].Input; in == nil || in.Arg != "url" || in.Kind != assistantkit.OptionInputURL {
		t.Fatalf("stored = %+v", in)
	}
	card, err := json.Marshal(st.cardList()[0].Options[0])
	if err != nil || !strings.Contains(string(card), `"input":{"kind":"url","label":"Page link"}`) {
		t.Fatalf("card = %s, %v", card, err)
	}

	_, err = h.tools.answer(t.Context(), AnswerRequest{ProposalID: id, OptionID: "import_page", Confirm: true}, nil)
	if ce, ok := errors.AsType[*ProposalConflictError](err); !ok || ce.Code != codeInputRequired || len(imported) != 0 || store.rows[0].Status != models.ProposalPending {
		t.Fatalf("without input: err = %v, imported = %v, status = %s", err, imported, store.rows[0].Status)
	}
	_, err = h.tools.answer(t.Context(), AnswerRequest{ProposalID: id, OptionID: "import_page", Input: " https://example.com/page ", Confirm: true}, nil)
	if err != nil || !slices.Equal(imported, []string{"https://example.com/page"}) || store.rows[0].Chosen != "import_page" {
		t.Fatalf("with input: err = %v, imported = %v, row = %+v", err, imported, store.rows[0])
	}
}
