package campaign_assistant

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// oneCampaign serves a single campaign; every other method is unused here.
type oneCampaign struct {
	repository.CampaignRepository
	c *models.Campaign
}

func (r oneCampaign) GetByID(context.Context, string) (*models.Campaign, error) { return r.c, nil }

// memMessages keeps the conversation so later turns see earlier ones.
type memMessages struct {
	repository.CampaignAssistantMessageRepository
	mu   sync.Mutex
	msgs []models.CampaignAssistantMessage
}

func (m *memMessages) CreateBatch(_ context.Context, msgs []*models.CampaignAssistantMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range msgs {
		m.msgs = append(m.msgs, *msg)
	}
	return nil
}

func (m *memMessages) ListRecentByCampaignID(context.Context, string, int) ([]models.CampaignAssistantMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]models.CampaignAssistantMessage(nil), m.msgs...), nil
}

// runnerHarness runs real campaign assistant turns against scripted models.
type runnerHarness struct {
	t         *testing.T
	g         *genkit.Genkit
	tools     *toolSet
	prompts   *prompts
	cfg       CampaignAssistantFlowConfig
	repos     CampaignAssistantRepos
	proposals *memProposals
	generated []content_plan.GeneratePostsRequest
	turns     int
}

func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()
	g := genkit.Init(t.Context())
	tools, err := defineTools(g)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPrompts(tools.reg)
	if err != nil {
		t.Fatal(err)
	}
	h := &runnerHarness{t: t, g: g, tools: tools, prompts: p, proposals: &memProposals{}}
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

// turn runs one user message with the model replaying script, and returns
// the response and every SSE event kind emitted.
func (h *runnerHarness) turn(instruction string, script ...fakemodel.Turn) (*CampaignAssistantResponse, []SSEEventKind) {
	h.t.Helper()
	h.turns++
	return h.turnWith(fakemodel.Define(h.g, "turn"+string(rune('a'+h.turns)), script...), instruction)
}

// turnWith runs one user message on an already defined scripted model, so the
// test can inspect what the model was sent.
func (h *runnerHarness) turnWith(m *fakemodel.Model, instruction string) (*CampaignAssistantResponse, []SSEEventKind) {
	h.t.Helper()
	fakemodel.Route(h.t.Context(), m, fakemodel.Slot{Flow: modelconfig.FlowCampaignAssistant, Slot: modelconfig.SlotOrchestrator})
	var events []SSEEventKind
	resp, err := runCampaignAssistant(h.t.Context(), h.g, CampaignAssistantRequest{CampaignID: "c1", Instruction: instruction},
		h.cfg, h.repos, h.prompts, h.tools, func(kind SSEEventKind, _ any) { events = append(events, kind) })
	if err != nil {
		h.t.Fatalf("turn %q: %v", instruction, err)
	}
	if m.Remaining() != 0 {
		h.t.Fatalf("turn %q left %d scripted model turns unused", instruction, m.Remaining())
	}
	return resp, events
}

func has(events []SSEEventKind, kind SSEEventKind) bool { return slices.Contains(events, kind) }

// The beta-test loop, end to end: a big request is proposed, "yes" runs it
// exactly as proposed, and a second "yes" does not run it again.
func TestRunner_ProposeThenConfirm(t *testing.T) {
	h := newRunnerHarness(t)

	resp, events := h.turn("Add 10 Threads posts",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 10}}),
		fakemodel.Answer(`{"explanation":"I've prepared 10 Threads posts for you to confirm.","action":"proposed"}`))
	if resp.Action != actionProposed || len(h.generated) != 0 || !has(events, SSEEventProposalCreated) {
		t.Fatalf("turn 1: action = %q, generated = %d, events = %v", resp.Action, len(h.generated), events)
	}
	if len(h.proposals.rows) != 1 {
		t.Fatalf("turn 1 stored %d proposals, want 1", len(h.proposals.rows))
	}
	id := h.proposals.rows[0].ID

	resp, events = h.turn("yes please",
		fakemodel.Tools(fakemodel.Call{Name: "confirmProposal", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"Done, 10 Threads posts added.","action":"posts_generated"}`))
	if resp.Action != actionPostsGenerated || len(h.generated) != 1 || h.generated[0].Count != 10 {
		t.Fatalf("turn 2: action = %q, generated = %+v", resp.Action, h.generated)
	}
	if !has(events, SSEEventProposalResolved) || h.proposals.rows[0].Status != models.ProposalConfirmed {
		t.Fatalf("turn 2: events = %v, proposal = %+v", events, h.proposals.rows[0])
	}

	resp, _ = h.turn("yes",
		fakemodel.Tools(fakemodel.Call{Name: "confirmProposal", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"That offer was already done.","action":"answered"}`))
	if resp.Action != actionAnswered || len(h.generated) != 1 {
		t.Fatalf("turn 3: action = %q, generated = %d, want the offer not to run again", resp.Action, len(h.generated))
	}
}

func TestRunner_ProposeThenDismiss(t *testing.T) {
	h := newRunnerHarness(t)
	h.turn("Add 10 Threads posts",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 10}}),
		fakemodel.Answer(`{"explanation":"Prepared for you to confirm.","action":"proposed"}`))
	id := h.proposals.rows[0].ID

	resp, events := h.turn("no, never mind",
		fakemodel.Tools(fakemodel.Call{Name: "dismissProposal", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"OK, dropped.","action":"answered"}`))
	if resp.Action != actionAnswered || len(h.generated) != 0 || h.proposals.rows[0].Status != models.ProposalDismissed || !has(events, SSEEventProposalResolved) {
		t.Fatalf("action = %q, generated = %d, proposal = %+v, events = %v", resp.Action, len(h.generated), h.proposals.rows[0], events)
	}
}

var farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// An offer made in one campaign cannot be confirmed from another.
func TestConfirmProposal_OtherCampaignIsGone(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := proposalState(store, &generated)
	_ = store.Create(t.Context(), &models.AssistantProposal{ID: "p-other", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c2",
		Tool: "generatePosts", Args: models.JSONMap{"platforms": []any{"Threads"}, "count": float64(10)}, ExpiresAt: farFuture})
	st.registry = newTestTools(t).reg
	out := runTool(t, st, "confirmProposal", map[string]any{"proposalId": "p-other"})
	if out.Reason != refuseOfferGone || generated != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, generated)
	}
}
