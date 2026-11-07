package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// memProposals keeps offers in memory, with the repository's resolve rule.
type memProposals struct {
	repository.AssistantProposalRepository
	mu   sync.Mutex
	rows []*models.AssistantProposal
}

func (m *memProposals) Create(_ context.Context, p *models.AssistantProposal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.Status = models.ProposalPending
	m.rows = append(m.rows, p)
	return nil
}

func (m *memProposals) GetByID(_ context.Context, id string) (*models.AssistantProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ID == id {
			cp := *p
			return &cp, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *memProposals) ListOpen(_ context.Context, kind models.AssistantScopeKind, id string, now time.Time) ([]models.AssistantProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.AssistantProposal
	for _, p := range m.rows {
		if p.ScopeKind == kind && p.ScopeID == id && p.Open(now) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (m *memProposals) Resolve(_ context.Context, id string, status models.AssistantProposalStatus, chosen, _ string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ID == id && p.Open(now) {
			p.Status, p.Chosen = status, chosen
			return true, nil
		}
	}
	return false, nil
}

type bulkIn struct {
	N int `json:"n"`
}

// brokenBulk is the count bulkThing fails on.
const brokenBulk = 13

// bulkPack has one gated tool: more than 3 items wait for a yes.
func bulkPack(ran *int) Pack {
	return Pack{
		Name: "bulk", Summary: "bulk things", Scopes: []models.AssistantScopeKind{models.AssistantScopeWorkspace},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "bulkThing", "Does n things.",
				assistantkit.Gated("bulkThing", assistantkit.Gate[bulkIn]{
					Needs:   func(_ context.Context, in bulkIn) bool { return in.N > 3 },
					Summary: func(in bulkIn) string { return fmt.Sprintf("Do %d things", in.N) },
				}, assistantkit.Wrap(func(ctx context.Context, in bulkIn) (int, error) {
					if in.N == brokenBulk {
						return 0, errors.New("bulk engine down")
					}
					*ran += in.N
					assistantkit.Touch(ctx, "thing", "")
					return in.N, nil
				})))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "bulk", Scopes: allScopes, Tier: assistantkit.TierProposal, Guide: "Does n things.", Tool: tool}}, nil
		},
	}
}

func proposalHarness(t *testing.T) (*harness, *memProposals, *int) {
	t.Helper()
	ran := new(int)
	store := &memProposals{}
	g := genkit.Init(t.Context())
	msgs := &memMessages{}
	repos := Repos{Messages: msgs, Views: testViews(), SavedViews: memSavedViews{}, Proposals: store}
	tools, err := defineTools(g, repos, []Pack{bulkPack(ran)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPrompts(tools)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, g: g, tools: tools, p: p, repos: repos, msgs: msgs}, store, ran
}

func TestProposals_ProposeThenConfirmInChat(t *testing.T) {
	h, store, ran := proposalHarness(t)
	home := viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`)

	resp, events, _, err := h.turn(home, "do 5 things",
		fakemodel.Tools(fakemodel.Call{Name: "bulkThing", Input: map[string]any{"n": 5}}),
		fakemodel.Answer(`{"explanation":"I prepared 5 things for you to confirm.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if *ran != 0 || len(store.rows) != 1 || store.rows[0].ScopeKind != models.AssistantScopeWorkspace || len(resp.Proposals) != 1 ||
		resp.Proposals[0].Summary != "Do 5 things" || !slices.Contains(events, SSEEventProposalCreated) {
		t.Fatalf("ran = %d, rows = %d, resp = %+v, events = %v", *ran, len(store.rows), resp.Proposals, events)
	}
	id := store.rows[0].ID

	// The offer is listed for the next turn, and "yes" runs it once.
	resp, events, m, err := h.turn(home, "yes",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"Done: 5 things.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastText(m.Requests()[0]), id+": Do 5 things") {
		t.Fatalf("the offer isn't in <view>: %q", lastText(m.Requests()[0]))
	}
	if *ran != 5 || store.rows[0].Status != models.ProposalConfirmed || !slices.Contains(events, SSEEventProposalResolved) ||
		!slices.Equal(resp.Touched, []assistantkit.Touched{{Kind: "thing"}}) {
		t.Fatalf("ran = %d, status = %s, events = %v, touched = %v", *ran, store.rows[0].Status, events, resp.Touched)
	}

	// A second yes finds it answered.
	if _, _, _, err := h.turn(home, "yes again",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": id}}),
		fakemodel.Answer(`{"explanation":"Already done.","action":"answered"}`)); err != nil || *ran != 5 {
		t.Fatalf("second yes: ran = %d, err = %v", *ran, err)
	}
}

// An offer answered before the turn failed is still announced.
func TestProposals_FailedTurnAnnouncesTheAnswer(t *testing.T) {
	h, store, _ := proposalHarness(t)
	home := viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`)
	if _, _, _, err := h.turn(home, "do 5 things",
		fakemodel.Tools(fakemodel.Call{Name: "bulkThing", Input: map[string]any{"n": 5}}),
		fakemodel.Answer(`{"explanation":"Ready when you are.","action":"answered"}`)); err != nil {
		t.Fatal(err)
	}
	// The model dismisses it, then fails before it replies.
	_, events, _, err := h.turn(home, "no",
		fakemodel.Tools(fakemodel.Call{Name: "dismissOffer", Input: map[string]any{"proposalId": store.rows[0].ID}}))
	if err == nil || store.rows[0].Status != models.ProposalDismissed || !slices.Contains(events, SSEEventProposalResolved) {
		t.Fatalf("err = %v, status = %s, events = %v", err, store.rows[0].Status, events)
	}
}

func TestProposals_AnswerFromTheCard(t *testing.T) {
	h, store, ran := proposalHarness(t)
	ctx := context.WithValue(t.Context(), logging.UserIDKey, "u1")
	future := time.Now().Add(time.Hour)
	store.rows = []*models.AssistantProposal{
		{ID: "mine", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(4)}, Summary: "Do 4 things", Status: models.ProposalPending, ExpiresAt: future},
		{ID: "gone", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "retiredTool", Summary: "Plan the campaign", Status: models.ProposalPending, ExpiresAt: future},
	}

	var events []SSEEventKind
	resp, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "mine", Confirm: true}, func(k SSEEventKind, _ any) { events = append(events, k) })
	if err != nil || *ran != 4 || resp.Explanation != "Done: Do 4 things" || !slices.Equal(events, []SSEEventKind{SSEEventProposalResolved, SSEEventComplete}) {
		t.Fatalf("resp = %+v, err = %v, ran = %d, events = %v", resp, err, *ran, events)
	}
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "mine", Confirm: true}, nil); !isConflict(err) {
		t.Fatalf("answering twice: %v, want a conflict", err)
	}
	// An offer for a tool no pack has any more can't run, only be dismissed.
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "gone", Confirm: true}, nil); !isConflict(err) {
		t.Fatalf("an offer for a retired tool: %v, want a conflict", err)
	}
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "gone"}, nil); err != nil || store.rows[1].Status != models.ProposalDismissed {
		t.Fatalf("dismissing it: %v", err)
	}
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "nope"}, nil); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("missing offer: %v", err)
	}
	// The answer joins the thread.
	if len(h.msgs.msgs) == 0 || h.msgs.msgs[0].Content != "Confirmed: Do 4 things" {
		t.Fatalf("thread = %+v", h.msgs.msgs)
	}
}

// A confirmed offer whose answer can't be stored still completes, marked
// unsaved: its work has run.
func TestProposals_AnswerFromTheCardUnsaved(t *testing.T) {
	h, store, ran := proposalHarness(t)
	h.msgs.fail = errors.New("db down")
	store.rows = []*models.AssistantProposal{
		{ID: "mine", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(4)},
			Summary: "Do 4 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	resp, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "mine", Confirm: true}, nil)
	if err != nil || *ran != 4 || !resp.Unsaved {
		t.Fatalf("resp = %+v, err = %v, ran = %d; want the work done and the reply unsaved", resp, err, *ran)
	}
}

func isConflict(err error) bool {
	_, ok := errors.AsType[*ProposalConflictError](err)
	return ok
}

// Work that fails after its offer was answered from the card still leaves the
// answer in the thread and tells the client the offer is resolved.
func TestProposals_AnswerFromTheCardWorkFails(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "broken", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(brokenBulk)},
			Summary: "Do 13 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	var events []SSEEventKind
	_, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "broken", Confirm: true}, func(k SSEEventKind, _ any) { events = append(events, k) })
	if err == nil || *ran != 0 || store.rows[0].Status != models.ProposalConfirmed {
		t.Fatalf("err = %v, ran = %d, status = %s", err, *ran, store.rows[0].Status)
	}
	if !slices.Equal(events, []SSEEventKind{SSEEventProposalResolved}) {
		t.Fatalf("events = %v", events)
	}
	if len(h.msgs.msgs) != 2 || h.msgs.msgs[0].Content != "Confirmed: Do 13 things" || !strings.Contains(h.msgs.msgs[1].Content, unfinishedAnswer) {
		t.Fatalf("thread = %+v", h.msgs.msgs)
	}
}
