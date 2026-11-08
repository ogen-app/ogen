package campaign_assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// oneCampaign serves a single campaign; every other method is unused here.
type oneCampaign struct {
	repository.CampaignRepository
	c *models.Campaign
}

func (r oneCampaign) GetByID(context.Context, string) (*models.Campaign, error) { return r.c, nil }

// memMessages keeps the conversation so later turns see earlier ones.
type memMessages struct {
	repository.AssistantMessageRepository
	mu   sync.Mutex
	msgs []models.AssistantMessage
}

func (m *memMessages) CreateBatch(_ context.Context, msgs []*models.AssistantMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range msgs {
		m.msgs = append(m.msgs, *msg)
	}
	return nil
}

func (m *memMessages) ListRecent(context.Context, models.AssistantScopeKind, string, int) ([]models.AssistantMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.msgs), nil
}

// runnerHarness runs real assistant turns in campaign c1's thread, with the
// campaign pack, against scripted models.
type runnerHarness struct {
	t         *testing.T
	g         *genkit.Genkit
	cfg       CampaignAssistantFlowConfig
	repos     CampaignAssistantRepos
	proposals *memProposals
	generated []content_plan.GeneratePostsRequest
	turns     int
	run       assistant.Runner
	// last is the scripted model of the latest turn.
	last *fakemodel.Model
}

func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()
	h := &runnerHarness{t: t, g: genkit.Init(t.Context()), proposals: &memProposals{}}
	c := timelineCampaign()
	c.ID = "c1"
	h.repos = CampaignAssistantRepos{Campaigns: oneCampaign{c: c}, Messages: &memMessages{}, Proposals: h.proposals}
	h.cfg = CampaignAssistantFlowConfig{
		MaxGeneratePosts: 10,
		GeneratePosts: func(_ context.Context, req content_plan.GeneratePostsRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			h.generated = append(h.generated, req)
			return &content_plan.ContentPlanResponse{Ideas: make([]content_plan.DraftPost, req.Count)}, nil
		},
	}
	return h
}

// init registers the assistant once the test has set the repos up; the
// pack reads them when it begins a turn.
func (h *runnerHarness) init() {
	if h.run != nil {
		return
	}
	pk, err := NewPack(h.cfg, h.repos)
	if err != nil {
		h.t.Fatal(err)
	}
	a, err := assistant.Init(h.g, assistant.FlowConfig{}, assistant.Repos{Messages: h.repos.Messages, Proposals: h.proposals}, pk)
	if err != nil {
		h.t.Fatal(err)
	}
	h.run = a.Run
}

// turn runs one user message with the model replaying script, and returns
// the response and every SSE event kind emitted.
func (h *runnerHarness) turn(instruction string, script ...fakemodel.Turn) (*assistant.Response, []assistant.SSEEventKind) {
	h.t.Helper()
	h.init()
	h.turns++
	return h.turnWith(fakemodel.Define(h.g, fmt.Sprint("turn", h.turns), script...), instruction)
}

// turnWith runs one user message on an already defined scripted model, so the
// test can inspect what the model was sent.
func (h *runnerHarness) turnWith(m *fakemodel.Model, instruction string) (*assistant.Response, []assistant.SSEEventKind) {
	h.t.Helper()
	h.init()
	h.last = m
	fakemodel.Route(h.t.Context(), m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"campaign.overview","scope":{"kind":"campaign","id":"c1"}}`))
	if err != nil {
		h.t.Fatal(err)
	}
	var events []assistant.SSEEventKind
	ctx := context.WithValue(h.t.Context(), logging.UserIDKey, "user-1")
	resp, err := h.run(ctx, assistant.Request{Instruction: instruction, View: &viewresolve.Resolved{View: v}},
		func(kind assistant.SSEEventKind, _ any) { events = append(events, kind) })
	if err != nil {
		h.t.Fatalf("turn %q: %v", instruction, err)
	}
	if m.Remaining() != 0 {
		h.t.Fatalf("turn %q left %d scripted model turns unused", instruction, m.Remaining())
	}
	return resp, events
}

// campaignResult is what the campaign pack reported for the turn.
func campaignResult(resp *assistant.Response) *CampaignAssistantResponse {
	r, _ := resp.Results[PackName].(*CampaignAssistantResponse)
	return r
}

// The beta-test loop, end to end: a big request is proposed, "yes" runs it
// exactly as proposed, and a second "yes" does not run it again.
func TestRunner_ProposeThenConfirm(t *testing.T) {
	h := newRunnerHarness(t)

	resp, events := h.turn("Add 10 Threads posts",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 10}}),
		fakemodel.Answer(`{"explanation":"I've prepared 10 Threads ideas for you to confirm.","action":"answered"}`))
	if len(resp.Proposals) != 1 || len(h.generated) != 0 || !slices.Contains(events, assistant.SSEEventProposalCreated) {
		t.Fatalf("turn 1: proposals = %+v, generated = %d, events = %v", resp.Proposals, len(h.generated), events)
	}
	id := h.proposals.rows[0].ID

	resp, events = h.turn("yes please",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"Done, 10 Threads ideas planned.","action":"answered"}`))
	if r := campaignResult(resp); r == nil || r.Action != actionPostsGenerated || len(h.generated) != 1 || h.generated[0].Count != 10 {
		t.Fatalf("turn 2: result = %+v, generated = %+v", r, h.generated)
	}
	if !slices.Contains(events, assistant.SSEEventProposalResolved) || !slices.Contains(events, assistant.SSEEventKind(SSEEventGeneratePostsComplete)) ||
		h.proposals.rows[0].Status != models.ProposalConfirmed {
		t.Fatalf("turn 2: events = %v, proposal = %+v", events, h.proposals.rows[0])
	}
	// The model got the work's own outcome back from confirmOffer.
	if out := toolOutput(h.last, "confirmOffer"); out["status"] != string(assistantkit.StatusOK) || resultField(out, "ideaCount") != float64(10) {
		t.Fatalf("turn 2: confirmOffer returned %v", out)
	}

	h.turn("yes",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"That offer was already done.","action":"answered"}`))
	if len(h.generated) != 1 {
		t.Fatalf("turn 3: generated = %d, want the offer not to run again", len(h.generated))
	}
}

func TestRunner_ProposeThenDismiss(t *testing.T) {
	h := newRunnerHarness(t)
	h.turn("Add 10 Threads posts",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 10}}),
		fakemodel.Answer(`{"explanation":"Prepared for you to confirm.","action":"answered"}`))
	id := h.proposals.rows[0].ID

	_, events := h.turn("no, never mind",
		fakemodel.Tools(fakemodel.Call{Name: "dismissOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"OK, dropped.","action":"answered"}`))
	if len(h.generated) != 0 || h.proposals.rows[0].Status != models.ProposalDismissed || !slices.Contains(events, assistant.SSEEventProposalResolved) {
		t.Fatalf("generated = %d, proposal = %+v, events = %v", len(h.generated), h.proposals.rows[0], events)
	}
}

// An offer whose work can't start because another heavy action already ran
// this message stays open, and confirming it in the next message runs it.
func TestRunner_HeavySlotTakenKeepsTheOfferOpen(t *testing.T) {
	h := newRunnerHarness(t)
	h.turn("Add 10 Threads posts",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 10}}),
		fakemodel.Answer(`{"explanation":"Prepared for you to confirm.","action":"answered"}`))
	id := h.proposals.rows[0].ID

	// A small plan runs first and takes the slot; the "yes" can't start.
	h.turn("add 2 more, and yes to the 10",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 2}}),
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"Added 2; confirm the 10 in your next message.","action":"answered"}`))
	if out := toolOutput(h.last, "confirmOffer"); out["status"] != string(assistantkit.StatusRefused) || out["reason"] != heavyConfirmLater {
		t.Fatalf("turn 2: confirmOffer returned %v", out)
	}
	if len(h.generated) != 1 || h.proposals.rows[0].Status != models.ProposalPending {
		t.Fatalf("turn 2: generated = %+v, proposal = %s", h.generated, h.proposals.rows[0].Status)
	}

	h.turn("yes",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"Done.","action":"answered"}`))
	if len(h.generated) != 2 || h.generated[1].Count != 10 || h.proposals.rows[0].Status != models.ProposalConfirmed {
		t.Fatalf("turn 3: generated = %+v, proposal = %s", h.generated, h.proposals.rows[0].Status)
	}
}

// toolOutput is the last response the model got from the named tool.
func toolOutput(m *fakemodel.Model, name string) map[string]any {
	var out map[string]any
	for _, tr := range m.ToolResponses() {
		if tr.Name == name {
			b, _ := json.Marshal(tr.Output)
			out = nil
			_ = json.Unmarshal(b, &out)
		}
	}
	return out
}

func resultField(out map[string]any, key string) any {
	r, _ := out["result"].(map[string]any)
	return r[key]
}
