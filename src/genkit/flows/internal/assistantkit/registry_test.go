package assistantkit

import (
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

func spec(g *genkit.Genkit, name, pack string, tier Tier, action *Action) Spec {
	return Spec{Name: name, Pack: pack, Scopes: []Scope{ScopeCampaign}, Tier: tier,
		Guide: "Call " + name + " when asked.", Action: action, Tool: tool(g, name)}
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
