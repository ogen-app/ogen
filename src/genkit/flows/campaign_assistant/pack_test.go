package campaign_assistant

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// memSink stores held-back calls as the campaign thread's offers, as the
// assistant's own sink does.
type memSink struct {
	store      *memProposals
	campaignID string
}

func (s memSink) Propose(ctx context.Context, p assistantkit.Proposal) (string, error) {
	if s.store == nil {
		return "", assistantkit.ErrNoProposalSink
	}
	args, err := jsonMap(p.Args)
	if err != nil {
		return "", err
	}
	var opts []models.ProposalOption
	for _, o := range p.Options {
		oargs, err := jsonMap(o.Args)
		if err != nil {
			return "", err
		}
		opt := models.ProposalOption{ID: o.ID, Title: o.Title, Tool: o.Tool, Args: oargs}
		if o.Input != nil {
			opt.Input = &models.ProposalOptionInput{Kind: o.Input.Kind, Label: o.Input.Label, Arg: o.Input.Arg}
		}
		opts = append(opts, opt)
	}
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	return id, s.store.Create(ctx, &models.AssistantProposal{
		ID: id, ScopeKind: models.AssistantScopeCampaign, ScopeID: s.campaignID,
		Tool: p.Tool, Args: args, Summary: p.Summary, Options: opts, ExpiresAt: time.Now().Add(24 * time.Hour),
	})
}

func jsonMap(v any) (models.JSONMap, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := models.JSONMap{}
	if string(b) == "null" {
		return m, nil
	}
	return m, json.Unmarshal(b, &m)
}

// bind gives ctx the turn's state and a sink for its offers.
func (st *requestState) bind(ctx context.Context) context.Context {
	store, _ := st.repos.Proposals.(*memProposals)
	return assistantkit.WithProposalSink(withRequestState(ctx, st), memSink{store: store, campaignID: st.campaignID})
}

func TestPack_RulesRender(t *testing.T) {
	p, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.rules, "{{") || strings.Contains(p.rules, "<no value>") {
		t.Fatalf("rules have unrendered fields:\n%s", p.rules)
	}
	for _, tool := range []string{"runContentPlan", "generatePosts", "draftPost", "enrichBrief", "checkBrief", "checkPostsConsistency"} {
		if !strings.Contains(p.rules, tool) {
			t.Errorf("rules don't mention %s", tool)
		}
	}
}

// The pack's block is the campaign's brief and its state; nothing in it
// waits for the user, since the offers are in <view>.
func TestPack_BeginBlock(t *testing.T) {
	p, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	c := timelineCampaign()
	brief, err := renderContext(c, "", p)
	if err != nil {
		t.Fatal(err)
	}
	snap := state.Build(c, nil, &state.Sources{Attached: 4, Ready: 2, Analysed: 1})
	snap.Calendar = &state.Calendar{Total: 3, Kinds: []state.KindCount{{Kind: "crowded day", Count: 2}, {Kind: "gap", Count: 1}},
		Top: []string{"Dec 8 has 3 posts on LinkedIn", "Dec 9 has 3 posts on LinkedIn"}}
	block, err := renderState(p, snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Sources: 4 assets attached, 2 ready, 1 analysed",
		"- Calendar: 3 problems (2 crowded day, 1 gap). First: Dec 8 has 3 posts on LinkedIn; Dec 9 has 3 posts on LinkedIn."} {
		if !strings.Contains(block, want) {
			t.Fatalf("state lacks %q:\n%s", want, block)
		}
	}
	if !strings.Contains(brief, "- Id: "+c.ID) || !strings.Contains(block, "<campaign_state>") {
		t.Fatalf("brief:\n%s\nstate:\n%s", brief, block)
	}
}

// A message about no campaign leaves the pack out of the turn, with a reason
// the model relays.
func TestPack_BeginNeedsACampaign(t *testing.T) {
	pk, err := NewPack(CampaignAssistantFlowConfig{}, CampaignAssistantRepos{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"home","scope":{"kind":"workspace"}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = pk.Begin(t.Context(), assistant.Turn{Instruction: "plan it", View: &viewresolve.Resolved{View: v}})
	if r, ok := err.(*assistantkit.Refusal); !ok || !strings.Contains(r.Reason, "one campaign") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// End reports the committed results: the write labels the turn over the
// review, both are attached, and the reply falls back to the first.
func TestPack_EndReportsTheWork(t *testing.T) {
	st := &requestState{campaignID: "c1", campaign: timelineCampaign(), briefReviewResult: briefReview(), briefResult: &BriefResult{Applied: true}}
	st.recordWrite(actionBriefEnriched)
	ctx, touched := assistant.NewTurnContext(t.Context(), viewcontext.Scope{Kind: models.AssistantScopeCampaign, ID: "c1"}, nil)
	res, err := end(withRequestState(ctx, st))
	if err != nil {
		t.Fatal(err)
	}
	r := res.Value.(*CampaignAssistantResponse)
	if r.Action != actionBriefEnriched || r.Brief == nil || !r.Brief.Applied || r.BriefReview == nil {
		t.Fatalf("result = %+v", r)
	}
	if res.Fallback == "" || !slices.ContainsFunc(touched(), func(tc assistantkit.Touched) bool { return tc.Kind == "campaign" && tc.ID == "c1" }) {
		t.Fatalf("fallback = %q, touched = %v", res.Fallback, touched())
	}

	// Nothing committed: only the state the turn read, with its starters.
	idle := &requestState{campaign: timelineCampaign(), snapshot: state.Snapshot{Goal: &state.Goal{Target: 20, Planned: 14, Room: 6}}}
	res, err = end(withRequestState(ctx, idle))
	if err != nil || res == nil || res.Fallback != "" {
		t.Fatalf("idle end = %+v, %v", res, err)
	}
	if r := res.Value.(*CampaignAssistantResponse); r.Action != actionAnswered || r.State == nil || r.State.Goal.Room != 6 ||
		len(r.Starters) != 1 || r.Starters[0].Label != "Draft 6 posts" {
		t.Fatalf("idle result = %+v", r)
	}
}
