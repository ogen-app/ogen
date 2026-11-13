package viewresolve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type fakeTitles struct {
	byKind map[viewcontext.EntityKind]map[string]string
	calls  int
	// asked counts the ids each kind's read received.
	asked map[viewcontext.EntityKind]int
}

func (f *fakeTitles) Titles(_ context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error) {
	f.calls++
	if f.asked == nil {
		f.asked = map[viewcontext.EntityKind]int{}
	}
	f.asked[kind] += len(ids)
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

// CampaignTitles reads the titles of the ids not ending in -other, which
// belong to another campaign.
func (f *fakeTitles) CampaignTitles(ctx context.Context, kind viewcontext.EntityKind, _ string, ids []string) (map[string]string, error) {
	got, err := f.Titles(ctx, kind, ids)
	maps.DeleteFunc(got, func(id, _ string) bool { return strings.HasSuffix(id, "-other") })
	return got, err
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

type fakeAssets struct {
	ids []string
	err error
	got models.AssetQuery
}

func (f *fakeAssets) QueryIDs(_ context.Context, q models.AssetQuery) ([]string, error) {
	f.got = q
	return f.ids, f.err
}

type fakePosts struct {
	ids   []string
	total int
	got   models.PostQuery
	asked bool
}

func (f *fakePosts) QueryIDs(_ context.Context, q models.PostQuery, limit int) ([]string, int, error) {
	f.got, f.asked = q, true
	return f.ids[:min(len(f.ids), limit)], f.total, nil
}

func titles() *fakeTitles {
	return &fakeTitles{byKind: map[viewcontext.EntityKind]map[string]string{
		viewcontext.KindCampaign: {"c1": "Spring launch"},
		viewcontext.KindIdea:     {"i1": "Pricing myths", "i2": "Founder story", "i3": "FAQ"},
		viewcontext.KindAsset:    {"a1": "Q3 deck"},
		viewcontext.KindPost:     {"p1": "Launch day", "p1-other": "Elsewhere"},
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
	r := New(ti, &fakeIdeas{}, &fakeAssets{}, &fakePosts{})
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
	r := New(ti, ideas, &fakeAssets{}, &fakePosts{})
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

	// A match deleted before the title read is dropped and stays in the total.
	r = New(ti, ideas, &fakeAssets{ids: []string{"a1", "a2"}}, &fakePosts{})
	got, err = r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"assets","scope":{"kind":"workspace"},
		"selection":{"kind":"asset","allMatching":true}}`))
	if err != nil || got.SelectionTotal != 2 || len(got.Selection) != 1 || got.Dropped != 1 {
		t.Fatalf("assets: %+v, %v; want a1 loaded of 2, a2 dropped", got, err)
	}
}

func TestResolve_AllMatchingAssets(t *testing.T) {
	var many []string
	ti := titles()
	for i := range 250 {
		id := fmt.Sprint("a", i)
		many = append(many, id)
		ti.byKind[viewcontext.KindAsset][id] = id
	}
	assets := &fakeAssets{ids: many}
	r := New(ti, &fakeIdeas{}, assets, &fakePosts{})
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"campaign.assets",
		"scope":{"kind":"campaign","id":"c1"},
		"selection":{"kind":"asset","allMatching":true},"filter":"text=pricing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if assets.got.ScopeCampaign != "c1" || assets.got.Text != "pricing" {
		t.Fatalf("query = %+v; want the filter scoped to the campaign", assets.got)
	}
	// The match is capped before the title read, which never sees the excess.
	if ti.asked[viewcontext.KindAsset] != MaxSelection {
		t.Fatalf("title read got %d asset ids, want %d", ti.asked[viewcontext.KindAsset], MaxSelection)
	}
	if len(got.Selection) != MaxSelection || got.SelectionTotal != 250 || got.Dropped != 0 {
		t.Fatalf("selection %d of %d, dropped %d", len(got.Selection), got.SelectionTotal, got.Dropped)
	}
}

// A campaign's posts list selects all matching within the thread's campaign.
func TestResolve_AllMatchingPosts(t *testing.T) {
	ti := titles()
	ti.byKind[viewcontext.KindPost]["p2"] = "Recap"
	posts := &fakePosts{ids: []string{"p1", "p2"}, total: 2}
	r := New(ti, &fakeIdeas{}, &fakeAssets{}, posts)
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"campaign.list",
		"scope":{"kind":"campaign","id":"c1"},
		"selection":{"kind":"post","allMatching":true},"filter":"status=failed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if posts.got.ScopeCampaign != "c1" || !slices.Equal(posts.got.Status.Any, []string{"failed"}) {
		t.Fatalf("query = %+v", posts.got)
	}
	if !slices.Equal(itemIDs(got.Selection), []string{"p1", "p2"}) || got.SelectionTotal != 2 {
		t.Fatalf("selection %+v total %d", got.Selection, got.SelectionTotal)
	}
}

// On the calendar the window narrows the filter: its days in the filter's
// time zone, its platforms and its campaign.
func TestResolve_AllMatchingCalendar(t *testing.T) {
	posts := &fakePosts{ids: []string{"p1"}, total: 1}
	r := New(titles(), &fakeIdeas{}, &fakeAssets{}, posts)
	_, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"calendar","scope":{"kind":"workspace"},
		"selection":{"kind":"post","allMatching":true},
		"filter":"platform=linkedin,x&scheduled_after=2026-11-01&tz=Europe/Kyiv",
		"window":{"from":"2026-10-26","to":"2026-11-08","platforms":["linkedin","instagram"],"campaignId":"c1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	q := posts.got
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	after, before := time.Date(2026, 11, 1, 0, 0, 0, 0, kyiv), time.Date(2026, 11, 9, 0, 0, 0, 0, kyiv)
	if !q.ScheduledAfter.Equal(after) || !q.ScheduledBefore.Equal(before) {
		t.Fatalf("days = %v .. %v, want %v .. %v", q.ScheduledAfter, q.ScheduledBefore, after, before)
	}
	if !slices.Equal(q.Platform.Any, []string{"linkedin"}) || q.ScopeCampaign != "c1" {
		t.Fatalf("query = %+v", q)
	}

	// A window that rules out the filter's platforms matches nothing,
	// without a read.
	posts = &fakePosts{}
	r = New(titles(), &fakeIdeas{}, &fakeAssets{}, posts)
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"campaign.calendar","scope":{"kind":"campaign","id":"c1"},
		"selection":{"kind":"post","allMatching":true},"filter":"platform=x",
		"window":{"platforms":["linkedin"]}}`))
	if err != nil || posts.asked || got.SelectionTotal != 0 {
		t.Fatalf("got %+v, asked %v, %v", got, posts.asked, err)
	}
}

// OnScreen asks for the given posts under the calendar's filter and window,
// and keeps them in the order asked.
func TestOnScreen(t *testing.T) {
	posts := &fakePosts{ids: []string{"p3", "p1"}, total: 2}
	r := New(titles(), &fakeIdeas{}, &fakeAssets{}, posts)
	v := parse(t, `{"version":1,"screen":"campaign.calendar","scope":{"kind":"campaign","id":"c1"},
		"filter":"status=draft&tz=Europe/Kyiv","window":{"from":"2026-04-20","to":"2026-04-26"}}`)
	got, err := r.OnScreen(t.Context(), v, []string{"p1", "p2", "p3"})
	if err != nil || !slices.Equal(got, []string{"p1", "p3"}) {
		t.Fatalf("on screen = %v, %v; want p1, p3 in the order asked", got, err)
	}
	q := posts.got
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	if !slices.Equal(q.IDs, []string{"p1", "p2", "p3"}) || q.ScopeCampaign != "c1" ||
		!slices.Equal(q.Status.Any, []string{"draft"}) ||
		!q.ScheduledAfter.Equal(time.Date(2026, 4, 20, 0, 0, 0, 0, kyiv)) ||
		!q.ScheduledBefore.Equal(time.Date(2026, 4, 27, 0, 0, 0, 0, kyiv)) {
		t.Fatalf("query = %+v", q)
	}

	// No ids, or a window the filter rules out, is none on screen without a
	// read.
	for _, tc := range []struct {
		raw string
		ids []string
	}{
		{`{"version":1,"screen":"calendar","scope":{"kind":"workspace"},"window":{"from":"2026-04-20","to":"2026-04-26"}}`, nil},
		{`{"version":1,"screen":"calendar","scope":{"kind":"workspace"},"filter":"platform=x","window":{"platforms":["linkedin"]}}`, []string{"p1"}},
	} {
		posts = &fakePosts{ids: []string{"p1"}}
		r = New(titles(), &fakeIdeas{}, &fakeAssets{}, posts)
		got, err := r.OnScreen(t.Context(), parse(t, tc.raw), tc.ids)
		if err != nil || len(got) != 0 || posts.asked {
			t.Fatalf("%s: got %v, asked %v, %v", tc.raw, got, posts.asked, err)
		}
	}
}

func TestResolve_Refusals(t *testing.T) {
	r := New(titles(), &fakeIdeas{}, &fakeAssets{}, &fakePosts{})
	cases := map[string]struct {
		raw      string
		field    string
		err      error
		assetErr error
	}{
		"scope gone": {`{"version":1,"screen":"post","scope":{"kind":"post","id":"p9"}}`, "", ErrScopeNotFound, nil},
		"asset scope gone": {`{"version":1,"screen":"campaign.assets","scope":{"kind":"campaign","id":"c9"},
			"selection":{"kind":"asset","allMatching":true}}`, "", ErrScopeNotFound, sql.ErrNoRows},
		"bad filter": {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},
			"selection":{"kind":"idea","allMatching":true},"filter":"pile=nope"}`, "filter", nil, nil},
		"no list query": {`{"version":1,"screen":"foundation.voices","scope":{"kind":"workspace"},
			"selection":{"kind":"voice","allMatching":true}}`, "selection.allMatching", nil, nil},
		"bad post filter": {`{"version":1,"screen":"campaign.list","scope":{"kind":"campaign","id":"c1"},
			"selection":{"kind":"post","allMatching":true},"filter":"status=lost"}`, "filter", nil, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := r
			if tc.assetErr != nil {
				r = New(titles(), &fakeIdeas{}, &fakeAssets{err: tc.assetErr}, &fakePosts{})
			}
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

// Ids of a kind whose titles can't be read can't be checked for the tenant,
// so they count as gone rather than passing through.
func TestResolve_UncheckedKindIsDropped(t *testing.T) {
	r := New(titles(), &fakeIdeas{}, &fakeAssets{}, &fakePosts{})
	got, err := r.Resolve(t.Context(), parse(t, `{"version":1,"screen":"foundation.voices","scope":{"kind":"workspace"},
		"selection":{"kind":"voice","ids":["v1"]}}`))
	if err != nil || len(got.Selection) != 0 || got.Dropped != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestItemsAndCheckFilter(t *testing.T) {
	r := New(titles(), &fakeIdeas{}, &fakeAssets{}, &fakePosts{})
	got, err := r.Items(t.Context(), viewcontext.KindIdea, []string{"i3", "gone", "i1"})
	if err != nil || !slices.Equal(itemIDs(got), []string{"i3", "i1"}) || got[0].Title != "FAQ" {
		t.Fatalf("items = %+v, %v", got, err)
	}
	if got, err := r.Items(t.Context(), viewcontext.KindVoice, []string{"v1"}); err != nil || len(got) != 0 {
		t.Fatalf("unchecked kind: %+v, %v", got, err)
	}
	got, err = r.CampaignItems(t.Context(), viewcontext.KindPost, "c1", []string{"p1-other", "p1"})
	if err != nil || !slices.Equal(itemIDs(got), []string{"p1"}) {
		t.Fatalf("campaign items = %+v, %v", got, err)
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
	if err := r.CheckFilter(viewcontext.KindPost, "status=draft,failed&scheduled=no"); err != nil {
		t.Fatal(err)
	}
	if ie, ok := errors.AsType[*viewcontext.InvalidError](r.CheckFilter(viewcontext.KindPost, "scheduled=maybe")); !ok || ie.Field != "filter" {
		t.Fatal("a scheduled value the list doesn't know passed")
	}
	if err := r.CheckFilter(viewcontext.KindFact, "x=1"); !errors.Is(err, errNoListQuery) {
		t.Fatalf("facts have no list filter: %v", err)
	}
}

func itemIDs(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
