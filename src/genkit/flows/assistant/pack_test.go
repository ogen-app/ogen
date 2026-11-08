package assistant

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// ideasPack is a stand-in pack: one tool that changes an idea.
func ideasPack() Pack {
	return Pack{
		Name:    "ideas",
		Summary: "find and triage ideas",
		Screens: []viewcontext.Screen{"ideas", "campaign.ideas"},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "triageIdea", "Marks an idea.",
				assistantkit.Wrap(func(ctx context.Context, _ struct{}) (string, error) {
					assistantkit.Touch(ctx, "idea", "i1")
					return "marked", nil
				}))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "ideas", Scopes: allScopes, Tier: assistantkit.TierReversible, Guide: "Marks an idea.", Tool: tool}}, nil
		},
	}
}

func campaignPack() Pack {
	return Pack{
		Name:    "campaign",
		Summary: "plan the campaign",
		Scopes:  []models.AssistantScopeKind{models.AssistantScopeCampaign},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "planCampaign", "Plans.",
				assistantkit.Wrap(func(context.Context, struct{}) (string, error) { return "planned", nil }))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "campaign", Scopes: allScopes, Tier: assistantkit.TierRead, Guide: "Plans.", Tool: tool}}, nil
		},
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func toolNames(req *ai.ModelRequest) []string {
	var out []string
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	return out
}

func viewOf(t *testing.T, raw string) *viewresolve.Resolved {
	t.Helper()
	v, err := viewcontext.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return &viewresolve.Resolved{View: v, Summary: "On " + string(v.Screen)}
}

func TestPacks_Defaults(t *testing.T) {
	ps := packList{ideasPack(), campaignPack()}
	if got := ps.defaults("campaign.ideas", models.AssistantScopeCampaign); !slices.Equal(got, []string{"ideas", "campaign"}) {
		t.Fatalf("campaign ideas = %v", got)
	}
	if got := ps.defaults("home", models.AssistantScopeWorkspace); len(got) != 0 {
		t.Fatalf("home = %v", got)
	}
	if got := ps.normalize([]string{"campaign", "ideas"}); !slices.Equal(got, []string{"ideas", "campaign"}) {
		t.Fatalf("normalize = %v; want registration order", got)
	}
	// none, ideas, campaign, ideas+campaign.
	if n := len(ps.combinations()); n != 4 {
		t.Fatalf("combinations = %v", ps.combinations())
	}
}

func TestDefineTools_Packs(t *testing.T) {
	g := genkit.Init(t.Context())
	if _, err := defineTools(g, Repos{}, nil); err != nil {
		t.Fatal(err)
	}
	bad := ideasPack()
	bad.Name = "other"
	if _, err := defineTools(genkit.Init(t.Context()), Repos{}, []Pack{bad}); err == nil || !strings.Contains(err.Error(), "belongs to pack") {
		t.Fatalf("a pack's tool in another pack: %v", err)
	}
}

func TestRun_ScreenPackLoadsByDefault(t *testing.T) {
	h := newHarness(t, ideasPack(), campaignPack())
	resp, _, m, err := h.turn(viewOf(t, `{"version":1,"screen":"ideas","scope":{"kind":"workspace"}}`), "hi",
		fakemodel.Answer(`{"explanation":"Hi.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(m.Requests()[0])
	if !slices.Contains(names, "triageIdea") || slices.Contains(names, "planCampaign") || !slices.Contains(names, "loadPack") {
		t.Fatalf("tools = %v; want core + ideas", names)
	}
	if !slices.Equal(resp.Packs, []string{"ideas"}) || resp.Touched == nil || len(resp.Touched) != 0 {
		t.Fatalf("packs = %v, touched = %#v", resp.Packs, resp.Touched)
	}
	if !strings.Contains(m.Requests()[0].Messages[0].Text(), "triageIdea") {
		t.Fatal("the system prompt doesn't guide the loaded pack's tools")
	}
}

func TestRun_LoadPackRestartsOnce(t *testing.T) {
	h := newHarness(t, ideasPack(), campaignPack())
	resp, events, m, err := h.turn(viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`), "mark the pricing idea",
		// The first call has no ideas tools, so it loads the pack…
		fakemodel.Tools(fakemodel.Call{Name: "loadPack", Input: map[string]any{"pack": "ideas"}}),
		// …and the restarted one uses it; a second load is refused.
		fakemodel.Tools(fakemodel.Call{Name: "triageIdea", Input: map[string]any{}}, fakemodel.Call{Name: "loadPack", Input: map[string]any{"pack": "campaign"}}),
		fakemodel.Answer(`{"explanation":"Marked it.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	reqs := m.Requests()
	if slices.Contains(toolNames(reqs[0]), "triageIdea") || !slices.Contains(toolNames(reqs[1]), "triageIdea") {
		t.Fatalf("tools: first %v, then %v", toolNames(reqs[0]), toolNames(reqs[1]))
	}
	if !slices.Contains(events, SSEEventPackLoaded) || !slices.Equal(resp.Packs, []string{"ideas"}) {
		t.Fatalf("events = %v, packs = %v", events, resp.Packs)
	}
	if !slices.Equal(resp.Touched, []assistantkit.Touched{{Kind: "idea", ID: "i1"}}) {
		t.Fatalf("touched = %v", resp.Touched)
	}
	var refused bool
	for _, tr := range m.ToolResponses() {
		if tr.Name == "loadPack" && strings.Contains(toJSON(t, tr.Output), "only one pack") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("a second loadPack in the turn was not refused")
	}
}

type subjectKey struct{}

// subjectPack acts on the thread's campaign: Begin binds it, its tool reads
// it and can hold work back, End reports what ran.
func subjectPack(ran *[]string) Pack {
	return Pack{
		Name:         "subject",
		Summary:      "work on the campaign",
		Scopes:       []models.AssistantScopeKind{models.AssistantScopeCampaign},
		Instructions: "Always name the campaign.",
		Begin: func(ctx context.Context, t Turn) (context.Context, string, error) {
			id := t.Subject(models.AssistantScopeCampaign)
			if id == "" {
				return nil, "", assistantkit.Refused("open a campaign first")
			}
			return context.WithValue(ctx, subjectKey{}, id), "<campaign>" + id + "</campaign>\n\n", nil
		},
		End: func(ctx context.Context) (*PackResult, error) {
			return &PackResult{Value: map[string]any{"campaign": ctx.Value(subjectKey{}), "ran": len(*ran)}, Fallback: "Worked on the campaign."}, nil
		},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "workOnSubject", "Works on the campaign.",
				assistantkit.Gated("workOnSubject", assistantkit.Gate[struct {
					Big bool `json:"big,omitempty"`
				}]{Needs: func(_ context.Context, in struct {
					Big bool `json:"big,omitempty"`
				}) bool {
					return in.Big
				}}, assistantkit.Wrap(func(ctx context.Context, _ struct {
					Big bool `json:"big,omitempty"`
				}) (string, error) {
					id, _ := ctx.Value(subjectKey{}).(string)
					*ran = append(*ran, id)
					return "done", nil
				})))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "subject", Scopes: allScopes, Tier: assistantkit.TierProposal, Guide: "Works on the campaign.", Tool: tool}}, nil
		},
	}
}

func TestRun_PackBeginsAndEnds(t *testing.T) {
	var ran []string
	h := newHarness(t, subjectPack(&ran))
	h.repos.Proposals = &memProposals{}
	h.tools.deps.Proposals = h.repos.Proposals
	campaign := viewOf(t, `{"version":1,"screen":"campaign.overview","scope":{"kind":"campaign","id":"c1"}}`)

	// The model gives no reply after the work: the pack's fallback stands in.
	resp, _, m, err := h.turn(campaign, "work on it",
		fakemodel.Tools(fakemodel.Call{Name: "workOnSubject", Input: map[string]any{}}),
		fakemodel.Answer(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req := m.Requests()[0]
	if !strings.Contains(lastText(req), "<campaign>c1</campaign>") || !strings.Contains(req.Messages[0].Text(), "Always name the campaign.") {
		t.Fatalf("prompt: system %q, user %q", req.Messages[0].Text(), lastText(req))
	}
	got, _ := resp.Results["subject"].(map[string]any)
	if !slices.Equal(ran, []string{"c1"}) || got["campaign"] != "c1" || resp.Explanation != "Worked on the campaign." {
		t.Fatalf("ran = %v, results = %v, explanation = %q", ran, resp.Results, resp.Explanation)
	}

	// Off a campaign, loading the pack is refused with its reason.
	resp, _, m, err = h.turn(viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`), "work on the campaign",
		fakemodel.Tools(fakemodel.Call{Name: "loadPack", Input: map[string]any{"pack": "subject"}}),
		fakemodel.Answer(`{"explanation":"Open a campaign first.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Packs) != 0 || slices.Contains(toolNames(m.Requests()[1]), "workOnSubject") || !strings.Contains(lastText(m.Requests()[1]), "open a campaign first") {
		t.Fatalf("packs = %v, tools = %v, prompt = %q", resp.Packs, toolNames(m.Requests()[1]), lastText(m.Requests()[1]))
	}

	// An offer made in the thread is confirmed from its card: the pack
	// begins for it with the offer's campaign.
	if _, _, _, err := h.turn(campaign, "do the big one",
		fakemodel.Tools(fakemodel.Call{Name: "workOnSubject", Input: map[string]any{"big": true}}),
		fakemodel.Answer(`{"explanation":"Ready when you are.","action":"answered"}`)); err != nil {
		t.Fatal(err)
	}
	store := h.repos.Proposals.(*memProposals)
	ctx := context.WithValue(t.Context(), logging.UserIDKey, "u1")
	resp, err = h.tools.answer(ctx, AnswerRequest{ProposalID: store.rows[0].ID, Confirm: true}, nil)
	if err != nil || !slices.Equal(ran, []string{"c1", "c1"}) || resp.Results["subject"] == nil {
		t.Fatalf("resp = %+v, err = %v, ran = %v", resp, err, ran)
	}
}

// Route returns what the model asked for without running it.
func TestRoute_ReturnsCallsUnrun(t *testing.T) {
	var ran []string
	h := newHarness(t, subjectPack(&ran))
	m := fakemodel.Define(h.g, "route", fakemodel.Tools(fakemodel.Call{Name: "workOnSubject", Input: map[string]any{"big": true}}))
	fakemodel.Route(t.Context(), m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})
	got, err := route(t.Context(), h.g, Request{Instruction: "work on it", View: viewOf(t, `{"version":1,"screen":"campaign.overview","scope":{"kind":"campaign","id":"c1"}}`)},
		FlowConfig{}, h.repos, h.p, h.tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Calls) != 1 || got.Calls[0].Name != "workOnSubject" || len(ran) != 0 || !slices.Equal(got.Packs, []string{"subject"}) {
		t.Fatalf("routing = %+v, ran = %v", got, ran)
	}
	if !strings.Contains(lastText(m.Requests()[0]), "<campaign>c1</campaign>") {
		t.Fatal("the pack's block isn't in the routed message")
	}
}

func TestRun_UIActionsStreamBeforeComplete(t *testing.T) {
	h := newHarness(t)
	resp, events, _, err := h.turn(viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`), "show me analysed ideas",
		fakemodel.Tools(fakemodel.Call{Name: "navigate", Input: map[string]any{"screen": "ideas"}}),
		fakemodel.Tools(fakemodel.Call{Name: "setFilter", Input: map[string]any{"filter": "origin=asset_analysis"}}),
		fakemodel.Answer(`{"explanation":"Here are the ideas from analyses.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.UIActions) != 2 || resp.UIActions[0].Type != UINavigate || resp.UIActions[1].Screen != "ideas" {
		t.Fatalf("ui actions = %+v", resp.UIActions)
	}
	n := len(events)
	if n < 3 || events[n-3] != SSEEventUIAction || events[n-2] != SSEEventUIAction || events[n-1] != SSEEventComplete {
		t.Fatalf("events = %v; want the ui actions right before complete", events)
	}
	if a, ok := h.payloads[n-3].(UIAction); !ok || a.Type != UINavigate {
		t.Fatalf("first ui_action = %#v", h.payloads[n-3])
	}
	// The stored model turn carries them, so the panel can replay them.
	if stored := h.msgs.msgs[1].Content; !strings.Contains(stored, `"uiActions"`) || !strings.Contains(stored, `"touched":[]`) {
		t.Fatalf("stored = %s", stored)
	}
}
