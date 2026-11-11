package assistant

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/savedviews"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// onScreen is a turn state for screen within scope, as the user's u1.
func onScreen(t *testing.T, raw string) (context.Context, *turnState) {
	t.Helper()
	v, err := viewcontext.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	st := newTurnState(&viewresolve.Resolved{View: v}, nil)
	ctx := context.WithValue(withTurnState(t.Context(), st), logging.UserIDKey, "u1")
	return ctx, st
}

const (
	campaignIdeas = `{"version":1,"screen":"campaign.ideas","scope":{"kind":"campaign","id":"c1"}}`
	workspaceHome = `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`
)

func refusal(t *testing.T, err error, contains string) {
	t.Helper()
	r, ok := errors.AsType[*assistantkit.Refusal](err)
	if !ok || !strings.Contains(r.Reason, contains) {
		t.Fatalf("err = %v, want a refusal mentioning %q", err, contains)
	}
}

func TestNavigate(t *testing.T) {
	h := newHarness(t)

	// A campaign screen defaults to the thread's campaign.
	ctx, st := onScreen(t, campaignIdeas)
	res, err := h.tools.navigate(ctx, NavigateInput{Screen: "campaign.calendar"})
	if err != nil || !strings.Contains(res.Done, `campaign "Spring launch"`) {
		t.Fatalf("res = %+v, %v", res, err)
	}
	// A detail screen needs its entity, which must exist.
	_, err = h.tools.navigate(ctx, NavigateInput{Screen: "asset"})
	refusal(t, err, "entityId")
	_, err = h.tools.navigate(ctx, NavigateInput{Screen: "asset", EntityID: "a9"})
	refusal(t, err, `no asset "a9"`)
	if _, err = h.tools.navigate(ctx, NavigateInput{Screen: "asset", EntityID: "a1"}); err != nil {
		t.Fatal(err)
	}
	got := st.uiActions()
	if len(got) != 2 || got[0].CampaignID != "c1" || got[1].Focus.ID != "a1" || st.currentScreen() != "asset" {
		t.Fatalf("actions = %+v, screen = %s", got, st.currentScreen())
	}

	// Outside a campaign thread, a campaign screen needs the campaign named.
	ctx, _ = onScreen(t, workspaceHome)
	_, err = h.tools.navigate(ctx, NavigateInput{Screen: "campaign.overview"})
	refusal(t, err, "which one")
	_, err = h.tools.navigate(ctx, NavigateInput{Screen: "campaign.overview", CampaignID: "c9"})
	refusal(t, err, `no campaign "c9"`)
	_, err = h.tools.navigate(ctx, NavigateInput{Screen: "billing"})
	refusal(t, err, `no "billing" screen`)
}

func TestSetFilterAndSelect(t *testing.T) {
	h := newHarness(t)

	// Home lists nothing to filter; after navigating to Ideas it does.
	ctx, st := onScreen(t, workspaceHome)
	_, err := h.tools.setFilter(ctx, SetFilterInput{Filter: "origin=manual"})
	refusal(t, err, "navigate to one first")
	if _, err := h.tools.navigate(ctx, NavigateInput{Screen: "ideas"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools.setFilter(ctx, SetFilterInput{Filter: "origin=manual"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.tools.setFilter(ctx, SetFilterInput{Filter: "pile=nope"})
	refusal(t, err, "unknown pile")

	res, err := h.tools.selectItems(ctx, SelectInput{IDs: []string{"i1", "gone", "i2"}})
	if err != nil || res.Done != "selecting 2 ideas" || !strings.Contains(res.Note, "1 of the ids") {
		t.Fatalf("select = %+v, %v", res, err)
	}
	_, err = h.tools.selectItems(ctx, SelectInput{IDs: []string{"gone"}})
	refusal(t, err, "none of those ideas")

	got := st.uiActions()
	if len(got) != 3 || got[1].Type != UISetFilter || *got[1].Filter != "origin=manual" || got[1].Screen != "ideas" ||
		got[2].Selection.Kind != viewcontext.KindIdea || len(got[2].Selection.IDs) != 2 {
		t.Fatalf("actions = %+v", got)
	}
}

func TestOpenSavedView(t *testing.T) {
	h := newHarness(t)
	ctx, st := onScreen(t, campaignIdeas)
	if _, err := h.tools.openSavedView(ctx, OpenSavedViewInput{SavedViewID: "sv1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools.openSavedView(ctx, OpenSavedViewInput{SavedViewID: "private"}); err != nil {
		t.Fatal(err)
	}
	// Someone else's private view reads as missing, as in the views list.
	_, err := h.tools.openSavedView(ctx, OpenSavedViewInput{SavedViewID: "theirs"})
	refusal(t, err, `no saved view "theirs"`)
	_, err = h.tools.openSavedView(ctx, OpenSavedViewInput{SavedViewID: "nope"})
	refusal(t, err, `no saved view "nope"`)

	got := st.uiActions()
	if len(got) != 2 || got[0].Screen != "campaign.ideas" || got[1].Screen != "campaign.assets" || got[1].CampaignID != "c1" {
		t.Fatalf("actions = %+v; want each opened on the campaign's own list", got)
	}
}

func TestOpenSavedViewSetsTheScreen(t *testing.T) {
	h := newHarness(t)
	// From home, the opened view's list takes filters and selections.
	ctx, st := onScreen(t, workspaceHome)
	if _, err := h.tools.openSavedView(ctx, OpenSavedViewInput{SavedViewID: "sv1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools.setFilter(ctx, SetFilterInput{Filter: "origin=manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools.selectItems(ctx, SelectInput{IDs: []string{"i1"}}); err != nil {
		t.Fatal(err)
	}
	// From another list, follow-ups target the opened one.
	if _, err := h.tools.openSavedView(ctx, OpenSavedViewInput{SavedViewID: "private"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools.selectItems(ctx, SelectInput{IDs: []string{"a1"}}); err != nil {
		t.Fatal(err)
	}
	got := st.uiActions()
	if len(got) != 5 || got[1].Screen != "ideas" || got[2].Screen != "ideas" ||
		got[4].Screen != "assets" || got[4].Selection.Kind != viewcontext.KindAsset {
		t.Fatalf("actions = %+v; want each follow-up on the opened list", got)
	}
}

// gatedViews holds the first idea lookup until release closes.
type gatedViews struct {
	memViews
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (v *gatedViews) Items(ctx context.Context, kind viewcontext.EntityKind, ids []string) ([]viewresolve.Item, error) {
	if kind == viewcontext.KindIdea {
		v.once.Do(func() { close(v.entered) })
		<-v.release
	}
	return v.memViews.Items(ctx, kind, ids)
}

func TestSelectIsPlannedApartFromNavigate(t *testing.T) {
	views := &gatedViews{memViews: testViews(), entered: make(chan struct{}), release: make(chan struct{})}
	tools := &toolSet{deps: Repos{Views: views}}
	ctx, st := onScreen(t, campaignIdeas)

	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := tools.selectItems(ctx, SelectInput{IDs: []string{"i1"}}); err != nil {
			t.Error(err)
		}
	})
	<-views.entered
	// The model's next tool in the same round navigates elsewhere while the
	// selection is still being checked.
	wg.Go(func() {
		if _, err := tools.navigate(ctx, NavigateInput{Screen: "campaign.calendar", CampaignID: "c2"}); err != nil {
			t.Error(err)
		}
	})
	time.Sleep(50 * time.Millisecond)
	close(views.release)
	wg.Wait()

	got := st.uiActions()
	if len(got) != 2 || got[0].Type != UISelect || got[0].Screen != "campaign.ideas" || got[0].CampaignID != "c1" ||
		got[1].Type != UINavigate || got[1].CampaignID != "c2" {
		t.Fatalf("actions = %+v; want the selection on c1's ideas, then the navigation", got)
	}
}

// listedViews answers List with views and records what it was asked.
type listedViews struct {
	views   []models.SavedView
	surface string
	counts  []string
	caller  savedviews.Caller
}

func (l *listedViews) List(_ context.Context, caller savedviews.Caller, surface string, counts []string, _ *time.Location) ([]models.SavedView, error) {
	l.caller, l.surface, l.counts = caller, surface, counts
	return l.views, nil
}

func TestListSavedViews(t *testing.T) {
	lister := &listedViews{views: []models.SavedView{
		{ID: "sv1", Name: "Needs analysis", Query: "ideas=unread", Shared: true, Valid: true, Counts: map[string]int{"total": 12}},
		{ID: "sv2", Name: "Old", Query: "bogus=1"},
	}}
	tools := &toolSet{deps: Repos{SavedViewLists: lister}}

	ctx, _ := onScreen(t, `{"version":1,"screen":"assets","scope":{"kind":"workspace"}}`)
	out, err := tools.listSavedViews(ctx, ListSavedViewsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if lister.surface != "assets" || lister.caller.UserID != "u1" || len(lister.counts) != 1 {
		t.Fatalf("asked %q as %+v for %v; want the list on screen, as the user, with its count", lister.surface, lister.caller, lister.counts)
	}
	if len(out.Views) != 1 || out.Views[0].Name != "Needs analysis" || out.Views[0].Filter != "ideas=unread" || *out.Views[0].Count != 12 {
		t.Fatalf("views = %+v", out.Views)
	}
	if !strings.Contains(out.Note, "1 of the saved views no longer match") {
		t.Fatalf("note = %q; an invalid view is left out and said so", out.Note)
	}

	// Inside a campaign the view opens on the campaign's list, so the
	// workspace count is left out.
	ctx, _ = onScreen(t, campaignIdeas)
	out, err = tools.listSavedViews(ctx, ListSavedViewsInput{List: "assets"})
	if err != nil || lister.surface != "assets" || lister.counts != nil || out.Views[0].Count != nil {
		t.Fatalf("campaign = %+v, %v (counts %v)", out, err, lister.counts)
	}
	if _, err := tools.listSavedViews(ctx, ListSavedViewsInput{}); err != nil || lister.surface != "ideas" {
		t.Fatalf("campaign.ideas lists %q, %v", lister.surface, err)
	}

	ctx, _ = onScreen(t, workspaceHome)
	_, err = tools.listSavedViews(ctx, ListSavedViewsInput{})
	refusal(t, err, "which list")
	_, err = tools.listSavedViews(ctx, ListSavedViewsInput{List: "posts"})
	refusal(t, err, "which list")
}

// The listed views reach the client beside the packs' results, so its card
// replays from history.
func TestListSavedViews_Result(t *testing.T) {
	lister := &listedViews{views: []models.SavedView{{ID: "sv1", Name: "Press", Query: "tag=press", Valid: true}}}
	tools := &toolSet{deps: Repos{SavedViewLists: lister}}
	ctx, st := onScreen(t, `{"version":1,"screen":"assets","scope":{"kind":"workspace"}}`)

	resp := &Response{}
	tools.finish(st, resp)
	if resp.Results != nil {
		t.Fatalf("results = %v before any list", resp.Results)
	}
	if _, err := tools.listSavedViews(ctx, ListSavedViewsInput{}); err != nil {
		t.Fatal(err)
	}
	resp = &Response{}
	tools.finish(st, resp)
	got, ok := resp.Results["savedViews"].(*ListSavedViewsOutput)
	if !ok || len(got.Views) != 1 || got.Views[0].Name != "Press" {
		t.Fatalf("results = %#v", resp.Results)
	}
}

// SetWindow sends a set_window for the analytics screen on view, the one a
// navigate opened included, and refuses any other screen; Selected reads
// the screen's selection of one kind.
func TestSetWindowAndSelected(t *testing.T) {
	view := viewcontext.ViewContext{Version: viewcontext.Version, Screen: "home", Scope: viewcontext.Scope{Kind: "workspace"},
		Selection: &viewcontext.Selection{Kind: viewcontext.KindPost, IDs: []string{"p1", "p2"}}}
	ctx, _ := NewViewTurnContext(t.Context(), view, nil)
	to := func(w viewcontext.Window) func(*viewcontext.Window) (viewcontext.Window, error) {
		return func(*viewcontext.Window) (viewcontext.Window, error) { return w, nil }
	}
	if _, _, err := SetWindow(ctx, to(viewcontext.Window{From: "2026-03-01"})); !errors.Is(err, ErrNoAnalyticsScreen) {
		t.Fatalf("home = %v", err)
	}
	st, _ := turnStateFrom(ctx)
	st.addUI(UIAction{Type: UINavigate, Screen: "analytics"})
	screen, _, err := SetWindow(ctx, to(viewcontext.Window{From: "2026-03-01", To: "2026-03-31"}))
	if err != nil || screen != "analytics" {
		t.Fatalf("screen = %q, %v", screen, err)
	}
	ui := st.uiActions()
	if len(ui) != 2 || ui[1].Type != UISetWindow || ui[1].Window.To != "2026-03-31" || Window(ctx).From != "2026-03-01" {
		t.Fatalf("ui = %+v", ui)
	}
	if got := Selected(ctx, viewcontext.KindPost); len(got) != 2 || Selected(ctx, viewcontext.KindIdea) != nil {
		t.Fatalf("selected = %v", got)
	}
}

// Windows set together each build on the other's: the merges run one at a
// time, so neither change is lost.
func TestSetWindow_ConcurrentMerges(t *testing.T) {
	view := viewcontext.ViewContext{Version: viewcontext.Version, Screen: "analytics", Scope: viewcontext.Scope{Kind: "workspace"}}
	ctx, _ := NewViewTurnContext(t.Context(), view, nil)
	merge := func(set func(*viewcontext.Window)) func(*viewcontext.Window) (viewcontext.Window, error) {
		return func(cur *viewcontext.Window) (viewcontext.Window, error) {
			var w viewcontext.Window
			if cur != nil {
				w = *cur
			}
			set(&w)
			return w, nil
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _, _ = SetWindow(ctx, merge(func(w *viewcontext.Window) { w.From, w.To = "2026-03-01", "2026-03-31" }))
	})
	wg.Go(func() {
		_, _, _ = SetWindow(ctx, merge(func(w *viewcontext.Window) { w.Platforms = []string{"linkedin"} }))
	})
	wg.Wait()
	if w := Window(ctx); w.From != "2026-03-01" || len(w.Platforms) != 1 {
		t.Fatalf("window = %+v", w)
	}
	// A merge that fails leaves the window and sends nothing.
	st, _ := turnStateFrom(ctx)
	if _, _, err := SetWindow(ctx, func(*viewcontext.Window) (viewcontext.Window, error) { return viewcontext.Window{}, errors.New("no") }); err == nil ||
		len(st.uiActions()) != 2 || Window(ctx).From != "2026-03-01" {
		t.Fatalf("failed merge = %v, ui %+v", err, st.uiActions())
	}
}

// OpenConnect sends an open_connect on the settings screen, and the turn is
// on settings after it.
func TestOpenConnect(t *testing.T) {
	view := viewcontext.ViewContext{Version: viewcontext.Version, Screen: "home", Scope: viewcontext.Scope{Kind: "workspace"}}
	ctx, _ := NewViewTurnContext(t.Context(), view, nil)
	if err := OpenConnect(ctx, UIConnect{Platform: "linkedin", AccountID: "sa1", Reconnect: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := turnStateFrom(ctx)
	ui := st.uiActions()
	if len(ui) != 1 || ui[0].Type != UIOpenConnect || ui[0].Screen != "settings" || ui[0].Connect.AccountID != "sa1" || st.currentScreen() != "settings" {
		t.Fatalf("ui = %+v", ui)
	}
}

// OpenAddSource sends an open_add_source on the campaign's Assets tab, and
// the turn is there after it.
func TestOpenAddSource(t *testing.T) {
	view := viewcontext.ViewContext{Version: viewcontext.Version, Screen: "campaign.overview", Scope: viewcontext.Scope{Kind: "campaign", ID: "c1"}}
	ctx, _ := NewViewTurnContext(t.Context(), view, nil)
	if err := OpenAddSource(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	st, _ := turnStateFrom(ctx)
	ui := st.uiActions()
	if len(ui) != 1 || ui[0].Type != UIOpenAddSource || ui[0].Screen != "campaign.assets" || ui[0].CampaignID != "c1" || st.currentScreen() != "campaign.assets" {
		t.Fatalf("ui = %+v", ui)
	}
	if err := OpenAddSource(t.Context(), "c1"); err == nil {
		t.Fatal("opened outside a turn")
	}
}

// A campaign's posts list can be ticked, with its own posts only, and
// filtered in the posts list's syntax.
func TestSelectPostsOnTheCampaignList(t *testing.T) {
	h := newHarness(t)
	ctx, st := onScreen(t, `{"version":1,"screen":"campaign.list","scope":{"kind":"campaign","id":"c1"}}`)
	res, err := h.tools.selectItems(ctx, SelectInput{IDs: []string{"p1", "p-other"}})
	if err != nil || res.Done != "selecting 1 post" || !strings.Contains(res.Note, "1 of the ids") {
		t.Fatalf("select = %+v, %v", res, err)
	}
	if got := st.uiActions(); len(got) != 1 || got[0].Selection.Kind != viewcontext.KindPost || !slices.Equal(got[0].Selection.IDs, []string{"p1"}) {
		t.Fatalf("actions = %+v", got)
	}
	res, err = h.tools.setFilter(ctx, SetFilterInput{Filter: "status=draft"})
	if err != nil || res.Done != "filtering the list by status=draft" {
		t.Fatalf("filter = %+v, %v", res, err)
	}
	if got := st.uiActions(); len(got) != 2 || got[1].Type != UISetFilter || got[1].Screen != "campaign.list" || got[1].CampaignID != "c1" {
		t.Fatalf("actions = %+v", got)
	}
}

// The workspace calendar ticks any of the workspace's posts and takes the
// posts filter; a foundation list ticks its items but has no filter.
func TestSelectOnTheCalendarAndFoundation(t *testing.T) {
	h := newHarness(t)
	ctx, st := onScreen(t, `{"version":1,"screen":"calendar","scope":{"kind":"workspace"}}`)
	res, err := h.tools.selectItems(ctx, SelectInput{IDs: []string{"p1", "p-other"}})
	if err != nil || res.Done != "selecting 2 posts" {
		t.Fatalf("select = %+v, %v", res, err)
	}
	if _, err := h.tools.setFilter(ctx, SetFilterInput{Filter: "platform=linkedin"}); err != nil {
		t.Fatal(err)
	}

	st.screen = "foundation.voices"
	res, err = h.tools.selectItems(ctx, SelectInput{IDs: []string{"v1"}})
	if err != nil || res.Done != "selecting 1 voice" {
		t.Fatalf("select voice = %+v, %v", res, err)
	}
	if got := st.uiActions(); got[len(got)-1].Selection.Kind != viewcontext.KindVoice {
		t.Fatalf("actions = %+v", got)
	}
	_, err = h.tools.setFilter(ctx, SetFilterInput{Filter: "x=1"})
	refusal(t, err, "no filter I can set")
}

// Without a campaign there are no campaign posts to tick.
func TestSelectPostsNeedsACampaign(t *testing.T) {
	h := newHarness(t)
	ctx, st := onScreen(t, workspaceHome)
	st.screen = "campaign.list"
	_, err := h.tools.selectItems(ctx, SelectInput{IDs: []string{"p1"}})
	refusal(t, err, "which campaign's posts")
}
