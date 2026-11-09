package assistant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// Pack is a domain's tools (ideas, assets, a campaign, …). A turn gets the
// core tools plus the packs of its screen and scope; loadPack adds one more
// when a request reaches beyond them. Few, stable combinations keep each one
// a cached prompt prefix.
type Pack struct {
	// Name is the pack every one of its specs names.
	Name string
	// Summary is the pack's line in loadPack's guide: what it lets you do.
	Summary string
	// Screens and Scopes load the pack by default.
	Screens []viewcontext.Screen
	Scopes  []models.AssistantScopeKind
	// Define registers the pack's tools on g.
	Define func(g *genkit.Genkit) ([]assistantkit.Spec, error)
	// Instructions are the pack's rules for the model, rendered into the
	// system prompt after the tool guide whenever the pack is loaded.
	Instructions string
	// Begin readies the pack's tools for one turn: it returns ctx carrying
	// what they share, and the pack's block for the turn ("" for none),
	// which follows the <view>. A Refusal leaves the pack out of the turn,
	// its reason told to the model. nil needs nothing.
	Begin func(ctx context.Context, t Turn) (context.Context, string, error)
	// End reports what the pack's tools did, once the reply is final. nil
	// reports nothing.
	End func(ctx context.Context) (*PackResult, error)
}

// Turn is what a pack's Begin is given.
type Turn struct {
	Instruction string
	View        *viewresolve.Resolved
	// Args is the input of the offer the pack begins for, when it does: an
	// offer made on another screen names its subject there.
	Args map[string]any
}

// Subject is the entity of kind the turn is about: the one an offer names as
// "<kind>Id", which is what confirming it works on, else the thread's when it
// is that kind's thread, else the item in focus when it is one; "" when none.
// A thread of kind with another of kind in focus is ambiguous — "this" means
// the focus, the thread is the other — so only an offer's own subject counts
// there.
func (t Turn) Subject(kind models.AssistantScopeKind) string {
	if offered, _ := t.Args[string(kind)+"Id"].(string); offered != "" {
		return offered
	}
	v := t.View.View
	focus := ""
	if f := t.View.Focus; f != nil && string(f.Kind) == string(kind) {
		focus = f.ID
	}
	if v.Scope.Kind == kind && v.Scope.ID != "" {
		if focus != "" && focus != v.Scope.ID {
			return ""
		}
		return v.Scope.ID
	}
	return focus
}

// PackResult is what a pack's tools did in a turn.
type PackResult struct {
	// Value is the pack's result for the client, under its name in
	// Response.Results; nil adds nothing.
	Value any
	// Fallback is the reply when the model gave none, e.g. it ran out of
	// tool rounds after the work was done.
	Fallback string
}

// packList is the registered packs, in registration order: the order their
// tools reach the model in, whatever order they loaded in.
type packList []Pack

func (ps packList) names() []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func (ps packList) has(name string) bool { return slices.Contains(ps.names(), name) }

// defaults returns the packs a turn on screen within scope starts with.
func (ps packList) defaults(screen viewcontext.Screen, scope models.AssistantScopeKind) []string {
	var out []string
	for _, p := range ps {
		if slices.Contains(p.Screens, screen) || slices.Contains(p.Scopes, scope) {
			out = append(out, p.Name)
		}
	}
	return out
}

// normalize orders loaded pack names by registration, so one set always
// renders the same prompt and tool list.
func (ps packList) normalize(loaded []string) []string {
	var out []string
	for _, n := range ps.names() {
		if slices.Contains(loaded, n) {
			out = append(out, n)
		}
	}
	return out
}

// combinations lists every distinct default set across screens and scopes,
// for the prewarm.
func (ps packList) combinations() [][]string {
	var out [][]string
	seen := map[string]bool{}
	for _, screen := range viewcontext.Screens() {
		for _, scope := range []models.AssistantScopeKind{models.AssistantScopeWorkspace, models.AssistantScopeCampaign, models.AssistantScopePost} {
			set := ps.defaults(screen, scope)
			if key := strings.Join(set, ","); !seen[key] {
				seen[key] = true
				out = append(out, set)
			}
		}
	}
	return out
}

// LoadPackInput names the pack to load.
type LoadPackInput struct {
	Pack string `json:"pack" jsonschema:"description=The pack to load"`
}

// LoadPackResult is never seen by the model: loading ends the step and the
// turn restarts with the pack's tools.
type LoadPackResult struct {
	Pack string `json:"pack"`
}

// loadPack restarts the turn with one more pack. It runs at most once a turn.
func (t *toolSet) loadPack(ctx context.Context, in LoadPackInput) (*LoadPackResult, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !t.packs.has(in.Pack):
		return nil, assistantkit.Refused(fmt.Sprintf("there is no %q pack", in.Pack), t.packs.names()...)
	case slices.Contains(st.loaded, in.Pack):
		return nil, assistantkit.Refused(fmt.Sprintf("the %s pack is already loaded; use its tools", in.Pack))
	case st.restarted:
		return nil, assistantkit.Refused("only one pack can be loaded per message; answer with the tools you have, and offer to continue in another message")
	}
	tc, ok := ctx.(*ai.ToolContext)
	if !ok {
		return nil, fmt.Errorf("loadPack called outside a tool call")
	}
	st.requestPack(in.Pack)
	return nil, tc.Interrupt(&ai.InterruptOptions{Metadata: map[string]any{"pack": in.Pack}})
}

func (ps packList) get(name string) (Pack, bool) {
	i := slices.IndexFunc(ps, func(p Pack) bool { return p.Name == name })
	if i < 0 {
		return Pack{}, false
	}
	return ps[i], true
}

// begin runs Begin for each pack in names the turn hasn't begun, in order,
// and returns ctx carrying them; a pack already begun is bound to ctx again.
// A pack that refuses leaves the turn, its reason noted for the model.
func (t *toolSet) begin(ctx context.Context, st *turnState, names []string) (context.Context, error) {
	return t.beginWith(ctx, st, names, st.turn)
}

func (t *toolSet) beginWith(ctx context.Context, st *turnState, names []string, turn Turn) (context.Context, error) {
	for _, name := range names {
		p, ok := t.packs.get(name)
		if !ok {
			continue
		}
		if b, ok := st.begunPack(name); ok {
			ctx = withBegun(ctx, b.ctx)
			continue
		}
		next, block := ctx, ""
		if p.Begin != nil {
			var err error
			next, block, err = p.Begin(ctx, turn)
			if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
				st.dropPack(name, fmt.Sprintf("The %s tools can't be used in this message: %s", name, r.Reason))
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("begin %s pack: %w", name, err)
			}
		}
		ctx = next
		st.addBegun(name, ctx, block)
	}
	return ctx, nil
}

// beginFor readies the pack of tool when the turn hasn't, for an offer made
// in an earlier turn or on another screen; args are the offer's input. A pack
// the turn has begun is bound to ctx again.
func (t *toolSet) beginFor(ctx context.Context, st *turnState, tool string, args map[string]any) (context.Context, error) {
	s, ok := t.reg.Lookup(tool)
	if !ok || s.Pack == corePack {
		return ctx, nil
	}
	turn := st.turn
	turn.Args = args
	ctx, err := t.beginWith(ctx, st, []string{s.Pack}, turn)
	if err != nil {
		return nil, err
	}
	if !st.hasBegun(s.Pack) {
		return nil, assistantkit.Refused(st.noteFor(s.Pack))
	}
	return ctx, nil
}

// begunCtx is ctx with the values of a pack begun on another context as a
// fallback. A pack begun inside a tool call — for an offer confirmed there —
// holds its state on that call's context, which the turn's later calls and
// the restarted model call don't derive from. ctx's own values come first,
// and its deadline and cancellation stand.
type begunCtx struct {
	context.Context
	begun context.Context
}

func (c *begunCtx) Value(key any) any {
	if v := c.Context.Value(key); v != nil {
		return v
	}
	return c.begun.Value(key)
}

// withBegun binds the pack state begun carries to ctx.
func withBegun(ctx, begun context.Context) context.Context {
	return &begunCtx{Context: ctx, begun: begun}
}

// end collects the results of the packs the turn began, by pack, and the
// first fallback reply. A pack that fails to report is logged and left out:
// its work has committed.
func (t *toolSet) end(st *turnState) (map[string]any, string) {
	var results map[string]any
	fallback := ""
	for _, b := range st.begunList() {
		p, ok := t.packs.get(b.name)
		if !ok || p.End == nil {
			continue
		}
		res, err := p.End(b.ctx)
		if err != nil {
			slog.ErrorContext(b.ctx, "pack result failed", logging.AttrComponent, logComponent, "pack", b.name, logging.AttrError, err)
			continue
		}
		if res == nil {
			continue
		}
		if res.Value != nil {
			if results == nil {
				results = map[string]any{}
			}
			results[b.name] = res.Value
		}
		fallback = cmp.Or(fallback, res.Fallback)
	}
	return results, fallback
}

func (t *toolSet) loadPackGuide() string {
	var b strings.Builder
	b.WriteString(`Call this when the request needs tools you don't have here — it is about another part of the app than this screen. Pass one pack; your tools are reloaded with it and you start again from the user's message. Only one pack per message. Packs:`)
	for _, p := range t.packs {
		fmt.Fprintf(&b, "\n  - %s: %s", p.Name, p.Summary)
	}
	return b.String()
}
