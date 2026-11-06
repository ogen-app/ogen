package assistant

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// corePack holds the tools every thread has, whatever the screen.
const corePack = "core"

var allScopes = []assistantkit.Scope{assistantkit.ScopeWorkspace, assistantkit.ScopeCampaign, assistantkit.ScopePost}

// selectionPage caps one getSelection call.
const selectionPage = 50

// toolSet is the flow's registered tools, in the order the model sees them.
type toolSet struct {
	reg *assistantkit.Registry
}

func (t *toolSet) all() []ai.ToolRef { return t.reg.Tools() }

type turnStateKey struct{}

// turnState is what one turn's tools read: the resolved view.
type turnState struct {
	view *viewresolve.Resolved
}

func withTurnState(ctx context.Context, st *turnState) context.Context {
	return context.WithValue(ctx, turnStateKey{}, st)
}

func turnStateFrom(ctx context.Context) (*turnState, error) {
	st, ok := ctx.Value(turnStateKey{}).(*turnState)
	if !ok || st.view == nil {
		return nil, fmt.Errorf("assistant tool called outside a turn")
	}
	return st, nil
}

// GetSelectionInput pages through the selection.
type GetSelectionInput struct {
	Offset int `json:"offset,omitempty" jsonschema:"description=Index of the first item to return; 0 for the start"`
	Limit  int `json:"limit,omitempty"  jsonschema:"description=How many items to return; at most 50"`
}

// SelectionPage is one page of the selected items.
type SelectionPage struct {
	Kind viewcontext.EntityKind `json:"kind,omitempty"`
	// Total is the selection's full size; Loaded how many items the server
	// loaded (at most 200). A "select all matching" larger than Loaded can
	// only be acted on as a whole, by its filter.
	Total       int                `json:"total"`
	Loaded      int                `json:"loaded"`
	AllMatching bool               `json:"allMatching,omitempty"`
	Filter      string             `json:"filter,omitempty"`
	Items       []viewresolve.Item `json:"items"`
	// Next is the offset of the next page; 0 when this is the last.
	Next int `json:"next,omitempty"`
}

func getSelection(ctx context.Context, in GetSelectionInput) (*SelectionPage, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	v := st.view
	sel := v.View.Selection
	if sel == nil {
		return nil, assistantkit.Refused("nothing is selected on the user's screen", "ask the user which items they mean")
	}
	limit := selectionPage
	if in.Limit > 0 {
		limit = min(in.Limit, selectionPage)
	}
	from := min(max(in.Offset, 0), len(v.Selection))
	to := min(from+limit, len(v.Selection))
	page := &SelectionPage{
		Kind:        sel.Kind,
		Total:       v.SelectionTotal,
		Loaded:      len(v.Selection),
		AllMatching: sel.AllMatching,
		Items:       v.Selection[from:to],
	}
	if sel.AllMatching {
		page.Filter = v.View.Filter
	}
	if to < len(v.Selection) {
		page.Next = to
	}
	return page, nil
}

// Focus is the screen around the selection.
type Focus struct {
	Screen      viewcontext.Screen  `json:"screen"`
	Scope       *viewresolve.Item   `json:"scope,omitempty"`
	Focus       *viewresolve.Item   `json:"focus,omitempty"`
	Filter      string              `json:"filter,omitempty"`
	SavedViewID string              `json:"savedViewId,omitempty"`
	Sort        string              `json:"sort,omitempty"`
	Window      *viewcontext.Window `json:"window,omitempty"`
	Visible     []viewresolve.Item  `json:"visible,omitempty"`
}

func getFocus(ctx context.Context, _ struct{}) (*Focus, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	v := st.view
	return &Focus{
		Screen:      v.View.Screen,
		Scope:       v.Scope,
		Focus:       v.Focus,
		Filter:      v.View.Filter,
		SavedViewID: v.View.SavedViewID,
		Sort:        v.View.Sort,
		Window:      v.View.Window,
		Visible:     v.Visible,
	}, nil
}

func defineTools(g *genkit.Genkit) (*toolSet, error) {
	selection := genkit.DefineTool(g, "getSelection",
		"Returns the items the user has selected on their screen, a page at a time, with their kind, id and title. Takes optional offset and limit.",
		assistantkit.Wrap(getSelection))
	focus := genkit.DefineTool(g, "getFocus",
		"Returns the user's screen: which screen, the thread's campaign or post, the item in focus, the list's filter, sort and saved view, the analytics window, and the first items on screen in order. Takes no arguments.",
		assistantkit.Wrap(getFocus))

	spec := func(tool ai.ToolRef, guide string) assistantkit.Spec {
		return assistantkit.Spec{Name: tool.Name(), Pack: corePack, Scopes: allScopes, Tier: assistantkit.TierRead, Guide: guide, Tool: tool}
	}
	reg, err := assistantkit.NewRegistry(
		spec(selection, `Call this when the user refers to what they selected ("these", "the selected ones", "all of them") and the items listed under <view> aren't enough — e.g. more than are listed, or you need their ids. Page with offset while the result has next.`),
		spec(focus, `Call this when you need more of the screen than the <view> summary gives: the filter, sort or saved view, the analytics window, or the order of the items on screen ("the third one").`),
	)
	if err != nil {
		return nil, err
	}
	return &toolSet{reg: reg}, nil
}
