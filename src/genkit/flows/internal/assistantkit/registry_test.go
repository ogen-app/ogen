package assistantkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
)

type noIn struct{}

func tool(g *genkit.Genkit, name string) ai.ToolRef {
	return genkit.DefineTool(g, name, name, func(*ai.ToolContext, noIn) (Outcome[string], error) {
		return OK("done"), nil
	})
}

// gatedTool is tool behind a gate that asks every time.
func gatedTool(g *genkit.Genkit, name string) ai.ToolRef {
	return genkit.DefineTool(g, name, name, Gated(name, Gate[noIn]{}, func(*ai.ToolContext, noIn) (Outcome[string], error) {
		return OK("done"), nil
	}))
}

// spec describes a tool of tier; a proposal-tier one is gated.
func spec(g *genkit.Genkit, name, pack string, tier Tier, action *Action) Spec {
	var t ai.ToolRef
	if tier == TierProposal {
		t = gatedTool(g, name)
	} else {
		t = tool(g, name)
	}
	return Spec{Name: name, Pack: pack, Scopes: []Scope{ScopeCampaign}, Tier: tier,
		Guide: "Call " + name + " when asked.", Action: action, Tool: t}
}

func names(refs []ai.ToolRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name()
	}
	return out
}

func TestRegistry_KeepsRegistrationOrderPerPack(t *testing.T) {
	g := genkit.Init(t.Context())
	r, err := NewRegistry(
		spec(g, "b", "campaign", TierRead, nil),
		spec(g, "a", "ideas", TierRead, nil),
		spec(g, "c", "campaign", TierProposal, &Action{Name: "c_done", Rule: "you called c"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(r.Tools()), ","); got != "b,a,c" {
		t.Fatalf("all tools = %s", got)
	}
	if got := strings.Join(names(r.Tools("campaign")), ","); got != "b,c" {
		t.Fatalf("campaign tools = %s", got)
	}
	if got := strings.Join(names(r.Tools("campaign", "ideas")), ","); got != "b,a,c" {
		t.Fatalf("campaign+ideas tools = %s, want registration order", got)
	}
	// Repeated calls yield the identical list: the grammar cache depends on it.
	if strings.Join(names(r.Tools("campaign", "ideas")), ",") != strings.Join(names(r.Tools("ideas", "campaign")), ",") {
		t.Fatal("tool order depends on pack argument order")
	}
	if s, ok := r.Lookup("c"); !ok || s.Tier != TierProposal {
		t.Fatalf("lookup c = %+v, %t", s, ok)
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("lookup of an unknown tool succeeded")
	}
}

func TestRegistry_RendersGuideAndActions(t *testing.T) {
	g := genkit.Init(t.Context())
	done := &Action{Name: "generated", Rule: "you generated something"}
	r, err := NewRegistry(
		spec(g, "gen", "campaign", TierProposal, done),
		spec(g, "regen", "campaign", TierProposal, done),
		spec(g, "read", "campaign", TierRead, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "- gen: Call gen when asked.\n- regen: Call regen when asked.\n- read: Call read when asked.\n"
	if got := r.Guide("campaign"); got != want {
		t.Fatalf("guide =\n%s\nwant\n%s", got, want)
	}
	acts := r.Actions()
	if len(acts) != 1 || acts[0].Name != "generated" {
		t.Fatalf("actions = %+v, want one deduplicated action", acts)
	}

	// A guide written over several lines stays on its tool's line, so a line
	// of it starting with "- " can't read as another tool.
	multi := spec(g, "multi", "ideas", TierRead, nil)
	multi.Guide = "Call this when asked.\n- not a tool\n\twrapped   here "
	r, err = NewRegistry(multi)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Guide(), "- multi: Call this when asked. - not a tool wrapped here\n"; got != want {
		t.Fatalf("guide = %q, want %q", got, want)
	}
}

func TestNewRegistry_RejectsInvalidSpecs(t *testing.T) {
	g := genkit.Init(t.Context())
	ok := spec(g, "ok", "campaign", TierRead, nil)
	cases := map[string]struct {
		specs []Spec
		want  string
	}{
		"duplicate": {[]Spec{ok, ok}, "registered twice"},
		"bad tier":  {[]Spec{{Name: "x", Pack: "p", Scopes: []Scope{ScopePost}, Tier: "maybe", Guide: "g", Tool: tool(g, "x")}}, `unknown tier "maybe"`},
		"no guide":  {[]Spec{{Name: "y", Pack: "p", Scopes: []Scope{ScopePost}, Tier: TierRead, Tool: tool(g, "y")}}, "guide is required"},
		"no scope":  {[]Spec{{Name: "z", Pack: "p", Tier: TierRead, Guide: "g", Tool: tool(g, "z")}}, "scope is required"},
		"name mismatch": {[]Spec{{Name: "w", Pack: "p", Scopes: []Scope{ScopePost}, Tier: TierRead, Guide: "g", Tool: tool(g, "other")}},
			`tool is named "other"`},
		"nil tool": {[]Spec{{Name: "u", Pack: "p", Scopes: []Scope{ScopePost}, Tier: TierRead, Guide: "g"}}, "tool is required"},
		"typed nil tool": {[]Spec{{Name: "t", Pack: "p", Scopes: []Scope{ScopePost}, Tier: TierRead, Guide: "g",
			Tool: (*ai.ToolDef[noIn, Outcome[string]])(nil)}}, "tool is required"},
		"half action": {[]Spec{{Name: "v", Pack: "p", Scopes: []Scope{ScopePost}, Tier: TierRead, Guide: "g", Tool: tool(g, "v"),
			Action: &Action{Name: "v_done"}}}, "action needs a name and a rule"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewRegistry(tc.specs...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOutcome_JSONShape(t *testing.T) {
	cases := map[string]struct {
		o    Outcome[int]
		want string
	}{
		"ok":       {OK(3), `{"status":"ok","result":3}`},
		"refused":  {Refuse[int]("LinkedIn has no text posts", "article", "carousel"), `{"status":"refused","reason":"LinkedIn has no text posts","alternatives":["article","carousel"]}`},
		"conflict": {Conflict[int]("over the goal", Option{ID: "fill", Title: "Fill the goal"}), `{"status":"conflict","reason":"over the goal","options":[{"id":"fill","title":"Fill the goal"}]}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Fatalf("json = %s, want %s", b, tc.want)
			}
			if tc.o.Succeeded() != (name == "ok") {
				t.Fatalf("succeeded = %t", tc.o.Succeeded())
			}
		})
	}
}

// Prepare runs a tool's claim before its offer is answered; a tool without
// one, or an unknown tool, claims nothing and releases nothing.
func TestRegistry_Prepare(t *testing.T) {
	g := genkit.Init(t.Context())
	type key struct{}
	released := false
	held := spec(g, "held", "campaign", TierProposal, nil)
	held.Prepare = func(ctx context.Context) (context.Context, func(), error) {
		return context.WithValue(ctx, key{}, true), func() { released = true }, nil
	}
	busy := spec(g, "busy", "campaign", TierProposal, nil)
	busy.Prepare = func(context.Context) (context.Context, func(), error) { return nil, nil, Refused("not now") }
	r, err := NewRegistry(held, busy, spec(g, "plain", "campaign", TierRead, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, err := r.Prepare(t.Context(), "held")
	if err != nil || ctx.Value(key{}) != true {
		t.Fatalf("held: ctx claim = %v, err = %v", ctx.Value(key{}), err)
	}
	release()
	if !released {
		t.Fatal("release didn't give the claim back")
	}
	if _, _, err := r.Prepare(t.Context(), "busy"); err == nil || err.Error() != "not now" {
		t.Fatalf("busy: err = %v", err)
	}
	for _, name := range []string{"plain", "unknown"} {
		ctx, release, err := r.Prepare(t.Context(), name)
		if err != nil || ctx != t.Context() || release == nil {
			t.Fatalf("%s: %v, %v", name, ctx, err)
		}
		release()
	}
}
