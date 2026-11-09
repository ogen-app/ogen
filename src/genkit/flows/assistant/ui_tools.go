package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/assets"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/savedviews"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// UIActionType names what a ui_action asks the client to do.
type UIActionType string

const (
	UINavigate      UIActionType = "navigate"
	UISetFilter     UIActionType = "set_filter"
	UISelect        UIActionType = "select"
	UIOpenSavedView UIActionType = "open_saved_view"
)

// UIAction drives the client's screen. The server has checked every
// reference in it; the client applies them in order when the turn completes.
type UIAction struct {
	Type UIActionType `json:"type"`
	// Screen is where the action applies: the destination of a navigate, the
	// list a filter, selection or saved view is for.
	Screen viewcontext.Screen `json:"screen"`
	// CampaignID is set for campaign-level screens.
	CampaignID string `json:"campaignId,omitempty"`
	// Focus is the entity a detail screen shows.
	Focus *viewcontext.Ref `json:"focus,omitempty"`
	// Filter replaces the list's filter; "" clears it.
	Filter *string `json:"filter,omitempty"`
	// Selection replaces the list's selection.
	Selection   *UISelection `json:"selection,omitempty"`
	SavedViewID string       `json:"savedViewId,omitempty"`
}

// UISelection is the items a select action ticks.
type UISelection struct {
	Kind viewcontext.EntityKind `json:"kind"`
	IDs  []string               `json:"ids"`
}

// UIResult is what a UI tool tells the model.
type UIResult struct {
	// Done says what will happen, in words.
	Done string `json:"done"`
	// Note is a caveat worth passing on.
	Note string `json:"note,omitempty"`
}

// uiApplies is the note every UI result carries: nothing moves until the
// reply arrives.
const uiApplies = "The user's screen changes when your reply arrives; describe it as done."

// NavigateInput is where to take the user.
type NavigateInput struct {
	Screen     string `json:"screen"               jsonschema:"description=The screen to open"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=The campaign of a campaign.* screen; defaults to this thread's campaign"`
	EntityID   string `json:"entityId,omitempty"   jsonschema:"description=The asset, post, voice, audience or series item a detail screen shows"`
}

func (t *toolSet) navigate(ctx context.Context, in NavigateInput) (*UIResult, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	st.planning.Lock()
	defer st.planning.Unlock()
	screen := viewcontext.Screen(in.Screen)
	if !screen.Valid() {
		return nil, assistantkit.Refused(fmt.Sprintf("there is no %q screen; pick one from navigate's list", in.Screen))
	}
	action := UIAction{Type: UINavigate, Screen: screen}
	label := string(screen)
	if screen.InCampaign() {
		id := cmp.Or(in.CampaignID, st.campaignID())
		if id == "" {
			return nil, assistantkit.Refused("a campaign screen needs a campaign; ask the user which one")
		}
		item, err := t.one(ctx, viewcontext.KindCampaign, id)
		if err != nil {
			return nil, err
		}
		action.CampaignID = id
		label += fmt.Sprintf(" of campaign %q", item.Title)
	}
	if kind, ok := screen.DetailKind(); ok {
		if in.EntityID == "" {
			return nil, assistantkit.Refused(fmt.Sprintf("the %s screen shows one %s; pass its id as entityId", screen, kind))
		}
		item, err := t.one(ctx, kind, in.EntityID)
		if err != nil {
			return nil, err
		}
		action.Focus = &viewcontext.Ref{Kind: kind, ID: item.ID}
		label += " for " + itemLabel(item)
	}
	st.addUI(action)
	return &UIResult{Done: "opening " + label, Note: uiApplies}, nil
}

// one reloads a single referenced entity, refusing one that doesn't exist.
func (t *toolSet) one(ctx context.Context, kind viewcontext.EntityKind, id string) (viewresolve.Item, error) {
	items, err := t.deps.Views.Items(ctx, kind, []string{id})
	if err != nil {
		return viewresolve.Item{}, err
	}
	if len(items) == 0 {
		return viewresolve.Item{}, assistantkit.Refused(fmt.Sprintf("there is no %s %q", kind, id))
	}
	return items[0], nil
}

// SetFilterInput is the list filter to apply.
type SetFilterInput struct {
	Filter string `json:"filter" jsonschema:"description=The filter as a URL query string in the list's syntax, e.g. origin=asset_analysis&text=pricing; empty clears it"`
}

func (t *toolSet) setFilter(ctx context.Context, in SetFilterInput) (*UIResult, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	st.planning.Lock()
	defer st.planning.Unlock()
	screen := st.currentScreen()
	kind, ok := screen.ListKind()
	if !ok {
		return nil, assistantkit.Refused("filters apply on the Ideas and Assets lists; navigate to one first")
	}
	if len(in.Filter) > viewcontext.MaxFilterLen {
		return nil, assistantkit.Refused("the filter is too long")
	}
	if err := t.deps.Views.CheckFilter(kind, in.Filter); err != nil {
		if ie, ok := errors.AsType[*viewcontext.InvalidError](err); ok {
			return nil, assistantkit.Refused("the list rejects that filter: " + ie.Reason)
		}
		return nil, err
	}
	st.addUI(UIAction{Type: UISetFilter, Screen: screen, CampaignID: st.currentCampaign(), Filter: &in.Filter})
	done := "clearing the filter"
	if in.Filter != "" {
		done = "filtering the list by " + in.Filter
	}
	return &UIResult{Done: done, Note: uiApplies}, nil
}

// SelectInput is the items to tick on the current list.
type SelectInput struct {
	IDs []string `json:"ids" jsonschema:"description=The ids of the items to select; they replace the selection"`
}

func (t *toolSet) selectItems(ctx context.Context, in SelectInput) (*UIResult, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	st.planning.Lock()
	defer st.planning.Unlock()
	screen := st.currentScreen()
	kind, ok := screen.ListKind()
	if !ok {
		return nil, assistantkit.Refused("items can be selected on the Ideas and Assets lists; navigate to one first")
	}
	if len(in.IDs) == 0 || len(in.IDs) > viewcontext.MaxSelectionIDs {
		return nil, assistantkit.Refused(fmt.Sprintf("select between 1 and %d items; for more, filter the list instead", viewcontext.MaxSelectionIDs))
	}
	items, err := t.deps.Views.Items(ctx, kind, in.IDs)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, assistantkit.Refused(fmt.Sprintf("none of those %ss exist", kind))
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	st.addUI(UIAction{Type: UISelect, Screen: screen, CampaignID: st.currentCampaign(), Selection: &UISelection{Kind: kind, IDs: ids}})
	res := &UIResult{Done: fmt.Sprintf("selecting %d %s", len(ids), plural(kind, len(ids))), Note: uiApplies}
	if missing := len(in.IDs) - len(ids); missing > 0 {
		res.Note = fmt.Sprintf("%d of the ids don't exist and were skipped. %s", missing, uiApplies)
	}
	return res, nil
}

// OpenSavedViewInput names the saved view to open.
type OpenSavedViewInput struct {
	SavedViewID string `json:"savedViewId" jsonschema:"description=The saved view's id"`
}

// savedViewScreens maps a saved-view surface to its list screen.
var savedViewScreens = map[string]viewcontext.Screen{ideas.SurfaceName: "ideas", assets.SurfaceName: "assets"}

func (t *toolSet) openSavedView(ctx context.Context, in OpenSavedViewInput) (*UIResult, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	st.planning.Lock()
	defer st.planning.Unlock()
	view, err := t.deps.SavedViews.GetByID(ctx, in.SavedViewID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assistantkit.Refused(fmt.Sprintf("there is no saved view %q", in.SavedViewID))
	}
	if err != nil {
		return nil, err
	}
	// A private view is its author's alone, as in the views list.
	user, _ := logging.UserIDFrom(ctx)
	if !view.Shared && (view.CreatedBy == nil || *view.CreatedBy != user) {
		return nil, assistantkit.Refused(fmt.Sprintf("there is no saved view %q", in.SavedViewID))
	}
	screen, ok := savedViewScreens[view.Surface]
	if !ok {
		return nil, assistantkit.Refused(fmt.Sprintf("saved views of %s can't be opened from here", view.Surface))
	}
	// Inside a campaign the view opens on the campaign's own list.
	campaign := st.currentCampaign()
	if campaign != "" {
		screen = "campaign." + screen
	}
	st.addUI(UIAction{Type: UIOpenSavedView, Screen: screen, CampaignID: campaign, SavedViewID: view.ID})
	return &UIResult{Done: fmt.Sprintf("opening the saved view %q", view.Name), Note: uiApplies}, nil
}

// ListSavedViewsInput names the list whose saved views to return.
type ListSavedViewsInput struct {
	List string `json:"list,omitempty" jsonschema:"enum=ideas,enum=assets,description=The list; defaults to the one on the user's screen"`
}

// SavedViewRef is one saved view: its name, the filter it stands for, and
// how many items it matches across the workspace.
type SavedViewRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Filter string `json:"filter"`
	Shared bool   `json:"shared"`
	// Count is left out inside a campaign, where the view opens on the
	// campaign's own list and the workspace count would mislead.
	Count *int `json:"count,omitempty"`
}

// ListSavedViewsOutput is the saved views of one list.
type ListSavedViewsOutput struct {
	List  string         `json:"list"`
	Views []SavedViewRef `json:"views"`
	Note  string         `json:"note,omitempty"`
}

// resultSavedViews is where listSavedViews's last list reaches the client,
// beside the packs' results.
const resultSavedViews = "savedViews"

// savedViewTotal is the count every saved-views surface carries.
const savedViewTotal = "total"

// listSurfaces maps a list kind to its saved-views surface.
var listSurfaces = map[viewcontext.EntityKind]string{viewcontext.KindIdea: ideas.SurfaceName, viewcontext.KindAsset: assets.SurfaceName}

func (t *toolSet) listSavedViews(ctx context.Context, in ListSavedViewsInput) (*ListSavedViewsOutput, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	surface := in.List
	if surface == "" {
		if kind, ok := st.currentScreen().ListKind(); ok {
			surface = listSurfaces[kind]
		}
	}
	if _, ok := savedViewScreens[surface]; !ok {
		return nil, assistantkit.Refused("which list? pass ideas or assets")
	}
	inCampaign := st.currentCampaign() != ""
	var counts []string
	if !inCampaign {
		counts = []string{savedViewTotal}
	}
	user, _ := logging.UserIDFrom(ctx)
	views, err := t.deps.SavedViewLists.List(ctx, savedviews.Caller{UserID: user}, surface, counts, time.UTC)
	if err != nil {
		return nil, err
	}
	out := &ListSavedViewsOutput{List: surface, Views: []SavedViewRef{}}
	invalid := 0
	for _, v := range views {
		if !v.Valid {
			invalid++
			continue
		}
		ref := SavedViewRef{ID: v.ID, Name: v.Name, Filter: v.Query, Shared: v.Shared}
		if n, ok := v.Counts[savedViewTotal]; ok && !inCampaign {
			ref.Count = new(n)
		}
		out.Views = append(out.Views, ref)
	}
	st.mu.Lock()
	st.savedViews = out
	st.mu.Unlock()
	var notes []string
	if invalid > 0 {
		notes = append(notes, fmt.Sprintf("%d of the saved views no longer match the list's filters and were left out.", invalid))
	}
	if inCampaign && len(out.Views) > 0 {
		notes = append(notes, "Inside a campaign a saved view opens on the campaign's own list, so no counts are given.")
	}
	out.Note = strings.Join(notes, " ")
	return out, nil
}

func plural(kind viewcontext.EntityKind, n int) string {
	if n == 1 || kind == viewcontext.KindSeries {
		return string(kind)
	}
	return string(kind) + "s"
}

// screenList is navigate's guide line of every screen.
func screenList() string {
	names := make([]string, 0, len(viewcontext.Screens()))
	for _, s := range viewcontext.Screens() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}
