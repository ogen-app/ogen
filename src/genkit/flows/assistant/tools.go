package assistant

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// corePack holds the tools every thread has, whatever the screen.
const corePack = "core"

var allScopes = []assistantkit.Scope{assistantkit.ScopeWorkspace, assistantkit.ScopeCampaign, assistantkit.ScopePost}

// selectionPage caps one getSelection call.
const selectionPage = 50

// toolSet is the flow's registered tools, in the order the model sees them,
// and what they read.
type toolSet struct {
	reg   *assistantkit.Registry
	packs packList
	deps  Repos
	// enforce checks the tenant's spend cap; nil checks nothing.
	enforce func(context.Context) error
}

// tools returns the core tools and those of the loaded packs, in registration
// order.
func (t *toolSet) tools(loaded []string) []ai.ToolRef {
	return t.reg.Tools(append([]string{corePack}, loaded...)...)
}

type turnStateKey struct{}

// turnState is what one turn's tools share. Tools of one model round run
// concurrently, so the mutable parts lock.
type turnState struct {
	view *viewresolve.Resolved

	// planning serializes the UI tools from reading the screen to adding
	// their action, so a filter or selection planned while navigate runs
	// targets one screen and campaign, in the order the actions apply.
	planning sync.Mutex

	mu sync.Mutex
	// screen and campaign follow the turn's navigations, so a filter set
	// after a navigate applies to the new list.
	screen   viewcontext.Screen
	campaign string
	// window is the analytics window a set_window in the turn asked for;
	// nil keeps the screen's.
	window *viewcontext.Window
	// loaded is the turn's packs, besides core; restarted is set once the
	// turn restarted with a pack loadPack asked for (requested).
	loaded    []string
	restarted bool
	requested string
	ui        []UIAction
	touches   assistantkit.Touches
	cards     []ProposalCard
	resolved  []ProposalResolved
	// savedViews is the last list listSavedViews returned, for the
	// client's card.
	savedViews *ListSavedViewsOutput
	// proposing serialises proposing; proposedKeys maps each call proposed
	// this turn (tool and args) to its offer's id.
	proposing    sync.Mutex
	proposedKeys map[string]string
	// emit streams pack events; nil outside a streamed turn.
	emit OnEventFunc
	// enforce checks the spend cap before a confirmed call that spends. It
	// is set for an offer answered from its card, which no model call
	// checked; nil checks nothing.
	enforce func(context.Context) error
	// turn is what the packs begin with; begun the packs that have, in
	// order, and notes why the others left the turn.
	turn  Turn
	begun []begunPack
	notes map[string]string
}

// begunPack is a pack readied for the turn: ctx carries its state, block is
// its part of the prompt.
type begunPack struct {
	name  string
	ctx   context.Context
	block string
}

func (st *turnState) hasBegun(name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.ContainsFunc(st.begun, func(b begunPack) bool { return b.name == name })
}

// begunPack returns the pack name the turn has begun.
func (st *turnState) begunPack(name string) (begunPack, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	i := slices.IndexFunc(st.begun, func(b begunPack) bool { return b.name == name })
	if i < 0 {
		return begunPack{}, false
	}
	return st.begun[i], true
}

func (st *turnState) addBegun(name string, ctx context.Context, block string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.begun = append(st.begun, begunPack{name: name, ctx: ctx, block: block})
}

func (st *turnState) begunList() []begunPack {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.begun)
}

// dropPack takes a pack that couldn't begin out of the turn.
func (st *turnState) dropPack(name, note string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loaded = slices.DeleteFunc(st.loaded, func(n string) bool { return n == name })
	if st.notes == nil {
		st.notes = map[string]string{}
	}
	st.notes[name] = note
}

func (st *turnState) noteFor(name string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.notes[name]
}

// packPrompt is what the begun packs add to the user's message, after the
// <view>: their blocks, then why any pack was left out.
func (st *turnState) packPrompt() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var b strings.Builder
	for _, p := range st.begun {
		b.WriteString(p.block)
	}
	for _, n := range slices.Sorted(maps.Keys(st.notes)) {
		b.WriteString(st.notes[n] + "\n\n")
	}
	return b.String()
}

func (st *turnState) addCard(c ProposalCard) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cards = append(st.cards, c)
}

// offeredNow reports whether the offer id was made this turn.
func (st *turnState) offeredNow(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.ContainsFunc(st.cards, func(c ProposalCard) bool { return c.ID == id })
}

func (st *turnState) addResolved(r ProposalResolved) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.resolved = append(st.resolved, r)
}

// unresolve forgets that the offer id was answered this turn: it is open
// again.
func (st *turnState) unresolve(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.resolved = slices.DeleteFunc(st.resolved, func(r ProposalResolved) bool { return r.ID == id })
}

// markStale records that the offer id answered this turn ran nothing,
// because of changes; replacedBy is the fresh offer of what still holds.
func (st *turnState) markStale(id string, changes []string, replacedBy string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r := ProposalResolved{ID: id, Status: resolvedStale, Reasons: changes, ReplacedBy: replacedBy}
	if i := slices.IndexFunc(st.resolved, func(r ProposalResolved) bool { return r.ID == id }); i >= 0 {
		st.resolved[i] = r
		return
	}
	st.resolved = append(st.resolved, r)
}

// resolvedAs is how the offer id was answered this turn; "" when it wasn't.
func (st *turnState) resolvedAs(id string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if i := slices.IndexFunc(st.resolved, func(r ProposalResolved) bool { return r.ID == id }); i >= 0 {
		return st.resolved[i].Status
	}
	return ""
}

func (st *turnState) cardList() []ProposalCard {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]ProposalCard(nil), st.cards...)
}

func (st *turnState) resolvedList() []ProposalResolved {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]ProposalResolved(nil), st.resolved...)
}

// bind gives ctx the turn's proposal sink, for the gated pack tools.
func (t *toolSet) bind(ctx context.Context, st *turnState) context.Context {
	return assistantkit.WithProposalSink(ctx, turnSink{st: st, repos: t.deps})
}

// finish copies what the turn did onto resp: the packs' results, and the UI
// actions, touches and offers.
func (t *toolSet) finish(st *turnState, resp *Response) {
	var fallback string
	resp.Results, fallback = t.end(st)
	st.mu.Lock()
	if st.savedViews != nil {
		if resp.Results == nil {
			resp.Results = map[string]any{}
		}
		resp.Results[resultSavedViews] = st.savedViews
	}
	st.mu.Unlock()
	if resp.Explanation == "" {
		resp.Explanation, resp.Action = fallback, actionAnswered
	}
	finish(st, resp)
}

func finish(st *turnState, resp *Response) {
	resp.UIActions = st.uiActions()
	resp.Touched = st.touches.List()
	resp.Proposals = st.cardList()
	resp.Resolved = st.resolvedList()
	st.mu.Lock()
	resp.Packs = append([]string(nil), st.loaded...)
	st.mu.Unlock()
}

func newTurnState(view *viewresolve.Resolved, loaded []string) *turnState {
	st := &turnState{view: view, loaded: loaded, turn: Turn{View: view}}
	st.resetScreen()
	return st
}

func (st *turnState) resetScreen() {
	st.screen, st.campaign = st.view.View.Screen, st.campaignID()
}

// campaignID is the thread's campaign; empty outside a campaign thread.
func (st *turnState) campaignID() string {
	if st.view.View.Scope.Kind == models.AssistantScopeCampaign {
		return st.view.View.Scope.ID
	}
	return ""
}

func (st *turnState) currentScreen() viewcontext.Screen {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.screen
}

func (st *turnState) currentCampaign() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.campaign
}

func (st *turnState) addUI(a UIAction) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ui = append(st.ui, a)
	// Opening a saved view puts its list on screen, as navigate does,
	// opening a connect flow the settings screen, and adding a source the
	// campaign's Assets tab.
	if a.Type == UINavigate || a.Type == UIOpenSavedView || a.Type == UIOpenConnect || a.Type == UIOpenAddSource {
		st.screen, st.campaign = a.Screen, a.CampaignID
	}
	if a.Type == UISetWindow {
		st.window = a.Window
	}
}

// currentWindow is the analytics window the turn reads: the one a
// set_window asked for, else the screen's.
func (st *turnState) currentWindow() *viewcontext.Window {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.window != nil {
		return st.window
	}
	return st.view.View.Window
}

func (st *turnState) uiActions() []UIAction {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]UIAction(nil), st.ui...)
}

func (st *turnState) loadedList() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.loaded)
}

func (st *turnState) requestPack(name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.requested = name
}

// restart takes the requested pack for the turn's second and last model
// call. What the first call did, the screen it planned included, stands:
// the second continues from its tool rounds.
func (st *turnState) restart(packs packList) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.restarted || st.requested == "" {
		return "", false
	}
	name := st.requested
	st.loaded = packs.normalize(append(st.loaded, name))
	st.restarted, st.requested = true, ""
	return name, true
}

func withTurnState(ctx context.Context, st *turnState) context.Context {
	return assistantkit.WithTouches(context.WithValue(ctx, turnStateKey{}, st), &st.touches)
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

// defineTools registers the core tools and every pack's on g. loadPack is
// registered only when there are packs to load.
func defineTools(g *genkit.Genkit, deps Repos, packs []Pack) (*toolSet, error) {
	t := &toolSet{packs: packs, deps: deps}
	spec := func(tool ai.ToolRef, guide string) assistantkit.Spec {
		return assistantkit.Spec{Name: tool.Name(), Pack: corePack, Scopes: allScopes, Tier: assistantkit.TierRead, Guide: guide, Tool: tool}
	}
	specs := []assistantkit.Spec{
		spec(genkit.DefineTool(g, "getSelection",
			"Returns the items the user has selected on their screen, a page at a time, with their kind, id and title. Takes optional offset and limit.",
			assistantkit.Wrap(getSelection)),
			`Call this when the user refers to what they selected ("these", "the selected ones", "all of them") and the items listed under <view> aren't enough — e.g. more than are listed, or you need their ids. Page with offset while the result has next.`),
		spec(genkit.DefineTool(g, "getFocus",
			"Returns the user's screen: which screen, the thread's campaign or post, the item in focus, the list's filter, sort and saved view, the analytics window, and the first items on screen in order. Takes no arguments.",
			assistantkit.Wrap(getFocus)),
			`Call this when you need more of the screen than the <view> summary gives: the filter, sort or saved view, the analytics window, or the order of the items on screen ("the third one").`),
		spec(genkit.DefineTool(g, "navigate",
			"Takes the user to a screen of the app. Pass the screen, campaignId for a campaign.* screen outside this campaign, and entityId for a detail screen.",
			assistantkit.Wrap(t.navigate)),
			`Call this when the user asks to go to, open or show a screen or an item — e.g. "take me to the calendar", "open the Q3 deck". Screens: `+screenList()+`. campaign.* screens default to this thread's campaign; detail screens (asset, post, analytics.post, foundation.voice, …) need entityId.`),
		spec(genkit.DefineTool(g, "setFilter",
			"Sets the filter of the Ideas, Assets or posts list or the calendar on the user's screen, as a URL query string in the list's syntax. An empty filter clears it.",
			assistantkit.Wrap(t.setFilter)),
			`Call this when the user asks to see a subset of the list on screen — e.g. "show me ideas from analyses", "only undecided ones", "just the failed posts". It applies to the list on screen, or to the one you just navigated to. Build on the current filter (getFocus) unless the user starts over. `+postsFilterSyntax),
		spec(genkit.DefineTool(g, "select",
			"Selects items on the list on the user's screen — the Ideas, Assets or posts list, the calendar, or the Foundation's voices, audiences, facts or series — replacing the selection. Takes their ids.",
			assistantkit.Wrap(t.selectItems)),
			`Call this when the user asks you to select or tick items on the list — e.g. "select the pricing ones". Pass ids you have from getSelection, getFocus or a pack's tools; never invent them.`),
		spec(genkit.DefineTool(g, "openSavedView",
			"Opens one of the user's saved views of the Ideas or Assets list. Takes the saved view's id.",
			assistantkit.Wrap(t.openSavedView)),
			`Call this when the user asks to open one of their saved views by name and you know its id; listSavedViews gives it.`),
	}
	if deps.SavedViewLists != nil {
		specs = append(specs, spec(genkit.DefineTool(g, "listSavedViews",
			"Lists the user's saved views (smart groups) of the Ideas or Assets list: their id, name, the filter each stands for and how many items it matches (not given inside a campaign).",
			assistantkit.Wrap(t.listSavedViews)),
			`Call this when the user asks which saved views or smart groups they have, or names one to open and you don't have its id. It defaults to the list on screen. To open one, call openSavedView with its id.`))
	}
	if deps.Proposals != nil {
		specs = append(specs,
			spec(genkit.DefineTool(g, "confirmOffer",
				"Runs an offer that is waiting for the user's answer, exactly as it was offered. Takes the offer's id, optionId when it has options, and input when the option picked needs a value.",
				func(ctx *ai.ToolContext, in AnswerProposalInput) (any, error) {
					out, err := t.confirmProposal(ctx, in)
					if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
						return assistantkit.Refuse[struct{}](r.Reason, r.Alternatives...), nil
					}
					return out, err
				}),
				`Call this when the user accepts an offer listed under "Waiting for the user's answer" in <view> — "yes", "go ahead", or by picking one of its options. It runs exactly what was offered and returns that work's result: report it as that tool's own result. Never call the offered tool again yourself. If several offers wait and it isn't clear which the user means, ask.`),
			spec(genkit.DefineTool(g, "dismissOffer",
				"Withdraws an offer that is waiting for the user's answer. Takes the offer's id. Nothing runs.",
				assistantkit.Wrap(t.dismissProposal)),
				`Call this when the user turns down an offer listed under "Waiting for the user's answer" — "no", "never mind". Nothing runs.`),
		)
	}
	if len(packs) > 0 {
		specs = append(specs, spec(genkit.DefineTool(g, "loadPack",
			"Loads the tools of another part of the app and starts this step again with them. Takes the pack's name.",
			assistantkit.Wrap(t.loadPack)), t.loadPackGuide()))
	}
	for _, p := range packs {
		if p.Name == corePack || p.Define == nil {
			return nil, fmt.Errorf("pack %q: a pack needs its own name and a Define", p.Name)
		}
		ps, err := p.Define(g)
		if err != nil {
			return nil, fmt.Errorf("define pack %s: %w", p.Name, err)
		}
		for _, s := range ps {
			if s.Pack != p.Name {
				return nil, fmt.Errorf("pack %s: tool %s belongs to pack %q", p.Name, s.Name, s.Pack)
			}
		}
		specs = append(specs, ps...)
	}
	reg, err := assistantkit.NewRegistry(specs...)
	if err != nil {
		return nil, err
	}
	t.reg = reg
	return t, nil
}
