package assistant

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

func TestGetSelection_Pages(t *testing.T) {
	ctx := withTurnState(t.Context(), &turnState{view: ideasView(t, 120, 412)})
	cases := []struct {
		in         GetSelectionInput
		first      string
		n, next    int
		allMatched bool
	}{
		{GetSelectionInput{}, "i0", 50, 50, true},
		{GetSelectionInput{Offset: 100, Limit: 10}, "i100", 10, 110, true},
		{GetSelectionInput{Offset: 110, Limit: 500}, "i110", 10, 0, true},
		{GetSelectionInput{Offset: 999}, "", 0, 0, true},
	}
	for _, tc := range cases {
		page, err := getSelection(ctx, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != tc.n || page.Next != tc.next || page.Total != 412 || page.Loaded != 120 || page.Filter != "origin=asset_analysis" {
			t.Fatalf("%+v: page = %d items, next %d, total %d, loaded %d, filter %q", tc.in, len(page.Items), page.Next, page.Total, page.Loaded, page.Filter)
		}
		if tc.n > 0 && page.Items[0].ID != tc.first {
			t.Fatalf("%+v: first = %s, want %s", tc.in, page.Items[0].ID, tc.first)
		}
	}
}

func TestGetSelection_NothingSelected(t *testing.T) {
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"ideas","scope":{"kind":"workspace"}}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := withTurnState(t.Context(), &turnState{view: &viewresolve.Resolved{View: v}})
	_, err = getSelection(ctx, GetSelectionInput{})
	if _, ok := errors.AsType[*assistantkit.Refusal](err); !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestGetFocus(t *testing.T) {
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"analytics","scope":{"kind":"workspace"},
		"focus":{"kind":"post","id":"p1"},"window":{"from":"2026-04-01","to":"2026-04-30"},"sort":"reach"}`))
	if err != nil {
		t.Fatal(err)
	}
	focus := &viewresolve.Item{Kind: viewcontext.KindPost, ID: "p1", Title: "Launch day"}
	ctx := withTurnState(t.Context(), &turnState{view: &viewresolve.Resolved{View: v, Focus: focus}})
	got, err := getFocus(ctx, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Screen != "analytics" || got.Focus.Title != "Launch day" || got.Window.From != "2026-04-01" || got.Sort != "reach" || got.Scope != nil {
		t.Fatalf("focus = %+v", got)
	}
	if _, err := getFocus(t.Context(), struct{}{}); err == nil {
		t.Fatal("getFocus ran outside a turn")
	}
}
