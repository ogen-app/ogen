package viewresolve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type fakeTitles struct {
	byKind map[viewcontext.EntityKind]map[string]string
	calls  int
}

func (f *fakeTitles) Titles(_ context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error) {
	f.calls++
	known, ok := f.byKind[kind]
	if !ok {
		return nil, repository.ErrUntitledKind
	}
	out := map[string]string{}
	for _, id := range ids {
		if t, ok := known[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

type fakeIdeas struct {
	ids   []string
	total int
	got   models.IdeaQuery
	limit int
}

func (f *fakeIdeas) QueryIDs(_ context.Context, q models.IdeaQuery, limit int) ([]string, int, error) {
	f.got, f.limit = q, limit
	return f.ids[:min(len(f.ids), limit)], f.total, nil
}

type fakeAssets struct{ ids []string }

func (f *fakeAssets) QueryIDs(context.Context, models.AssetQuery) ([]string, error) {
	return f.ids, nil
}

func titles() *fakeTitles {
	return &fakeTitles{byKind: map[viewcontext.EntityKind]map[string]string{
		viewcontext.KindCampaign: {"c1": "Spring launch"},
		viewcontext.KindIdea:     {"i1": "Pricing myths", "i2": "Founder story", "i3": "FAQ"},
		viewcontext.KindAsset:    {"a1": "Q3 deck"},
		viewcontext.KindPost:     {},
	}}
}

func parse(t *testing.T, raw string) *viewcontext.ViewContext {
	t.Helper()
	v, err := viewcontext.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestResolve_SelectionByIDs(t *testing.T) {
	ti := titles()
	r := New(ti, &fakeIdeas{}, &fakeAssets{})
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"campaign.ideas",
		"scope":{"kind":"campaign","id":"c1"},
		"focus":{"kind":"asset","id":"a1"},
		"selection":{"kind":"idea","ids":["i2","gone","i1"]},
		"visible":{"kind":"idea","ids":["i1","i2","i3"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope.Title != "Spring launch" || got.Focus.Title != "Q3 deck" {
		t.Fatalf("scope %+v focus %+v", got.Scope, got.Focus)
	}
	if ids := itemIDs(got.Selection); !slices.Equal(ids, []string{"i2", "i1"}) || got.SelectionTotal != 2 || got.Dropped != 1 {
		t.Fatalf("selection %v total %d dropped %d; want the live ids in order", ids, got.SelectionTotal, got.Dropped)
	}
	if len(got.Visible) != 3 {
		t.Fatalf("visible = %v", got.Visible)
	}
	// One read per kind: campaign, asset, idea (selection and visible share).
	if ti.calls != 3 {
		t.Fatalf("title reads = %d, want 3", ti.calls)
	}
	want := `On campaign.ideas in campaign "Spring launch" · focused on asset "Q3 deck" · 2 ideas selected (1 more no longer exist)`
	if got.Summary != want {
		t.Fatalf("summary = %q\nwant      %q", got.Summary, want)
	}
}

func TestResolve_AllMatching(t *testing.T) {
	var many []string
	for i := range 250 {
		many = append(many, fmt.Sprint("i", i))
	}
	ti := titles()
	for _, id := range many {
		ti.byKind[viewcontext.KindIdea][id] = id
	}
	ideas := &fakeIdeas{ids: many, total: 412}
	r := New(ti, ideas, &fakeAssets{})
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"campaign.ideas",
		"scope":{"kind":"campaign","id":"c1"},
		"selection":{"kind":"idea","allMatching":true},"filter":"origin=asset_analysis"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ideas.got.ScopeCampaign != "c1" || ideas.limit != MaxSelection || !ideas.got.Origin.Active() {
		t.Fatalf("query = %+v limit %d; want the filter scoped to the campaign", ideas.got, ideas.limit)
	}
	if len(got.Selection) != MaxSelection || got.SelectionTotal != 412 {
		t.Fatalf("selection %d of %d", len(got.Selection), got.SelectionTotal)
	}
	want := `On campaign.ideas in campaign "Spring launch" · 412 ideas selected (all matching the filter; the first 200 loaded)`
	if got.Summary != want {
		t.Fatalf("summary = %q", got.Summary)
	}

	// Assets cap the same way; the total is the whole match.
	r = New(ti, ideas, &fakeAssets{ids: []string{"a1", "a2"}})
	got, err = r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"assets","scope":{"kind":"workspace"},
		"selection":{"kind":"asset","allMatching":true}}`))
	if err != nil || got.SelectionTotal != 2 || len(got.Selection) != 1 {
		t.Fatalf("assets: %+v, %v; want a1 loaded of 2", got, err)
	}
}

func TestResolve_Refusals(t *testing.T) {
	r := New(titles(), &fakeIdeas{}, &fakeAssets{})
	cases := map[string]struct {
		raw   string
		field string
		err   error
	}{
		"scope gone": {`{"version":1,"screen":"post","scope":{"kind":"post","id":"p9"}}`, "", ErrScopeNotFound},
		"bad filter": {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},
			"selection":{"kind":"idea","allMatching":true},"filter":"pile=nope"}`, "filter", nil},
		"no list query": {`{"version":1,"screen":"campaign.list","scope":{"kind":"campaign","id":"c1"},
			"selection":{"kind":"post","allMatching":true}}`, "selection.allMatching", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.Resolve(t.Context(), parse(t, tc.raw))
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if ie, ok := errors.AsType[*viewcontext.InvalidError](err); !ok || ie.Field != tc.field {
				t.Fatalf("err = %v, want field %q", err, tc.field)
			}
		})
	}
}

func TestResolve_UntitledKindPassesThrough(t *testing.T) {
	r := New(titles(), &fakeIdeas{}, &fakeAssets{})
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"foundation.voices","scope":{"kind":"workspace"},
		"selection":{"kind":"voice","ids":["v1"]}}`))
	if err != nil || len(got.Selection) != 1 || got.Selection[0].Title != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if want := "On foundation.voices · 1 voice selected"; got.Summary != want {
		t.Fatalf("summary = %q", got.Summary)
	}
}

func TestItemsAndCheckFilter(t *testing.T) {
	r := New(titles(), &fakeIdeas{}, &fakeAssets{})
	got, err := r.Items(t.Context(), viewcontext.KindIdea, []string{"i3", "gone", "i1"})
	if err != nil || !slices.Equal(itemIDs(got), []string{"i3", "i1"}) || got[0].Title != "FAQ" {
		t.Fatalf("items = %+v, %v", got, err)
	}
	if got, err := r.Items(t.Context(), viewcontext.KindVoice, []string{"v1"}); err != nil || len(got) != 1 {
		t.Fatalf("untitled kind: %+v, %v", got, err)
	}

	if err := r.CheckFilter(viewcontext.KindIdea, "origin=asset_analysis"); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckFilter(viewcontext.KindAsset, ""); err != nil {
		t.Fatal(err)
	}
	if ie, ok := errors.AsType[*viewcontext.InvalidError](r.CheckFilter(viewcontext.KindIdea, "pile=nope")); !ok || ie.Field != "filter" {
		t.Fatal("a pile the list doesn't know passed")
	}
	if err := r.CheckFilter(viewcontext.KindPost, "x=1"); !errors.Is(err, errNoListQuery) {
		t.Fatalf("posts have no list filter: %v", err)
	}
}

func itemIDs(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
