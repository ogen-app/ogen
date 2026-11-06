package assistant

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
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

func (t *toolSet) loadPackGuide() string {
	var b strings.Builder
	b.WriteString(`Call this when the request needs tools you don't have here — it is about another part of the app than this screen. Pass one pack; your tools are reloaded with it and you start again from the user's message. Only one pack per message. Packs:`)
	for _, p := range t.packs {
		fmt.Fprintf(&b, "\n  - %s: %s", p.Name, p.Summary)
	}
	return b.String()
}
