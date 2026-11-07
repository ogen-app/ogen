package assistant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
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
