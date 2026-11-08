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

// brokenBulk is the count bulkThing fails on; goneBulk the one it refuses
// once confirmed, as work whose subject went away while it waited.
const (
	brokenBulk = 13
	goneBulk   = 14
)

// bulkPack has one gated tool: more than 3 items wait for a yes. It streams
// a bulk_progress event as it runs.
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
					if in.N == goneBulk {
						return 0, assistantkit.Refused("those things are gone")
					}
					Emit(ctx, "bulk_progress", in.N)
					*ran += in.N
					assistantkit.Touch(ctx, "thing", "")
					return in.N, nil
				})))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "bulk", Scopes: allScopes, Tier: assistantkit.TierProposal, Guide: "Does n things.", Tool: tool}}, nil
		},
	}
}

// proposalHarness builds the assistant with one proposal-tier pack; foreign,
// when given, answers the offers another assistant made.
func proposalHarness(t *testing.T, foreign ...func(context.Context, AnswerRequest) (any, error)) (*harness, *memProposals, *int) {
	t.Helper()
	ran := new(int)
	store := &memProposals{}
	g := genkit.Init(t.Context())
	msgs := &memMessages{}
	repos := Repos{Messages: msgs, Views: testViews(), SavedViews: memSavedViews{}, Proposals: store}
	if len(foreign) > 0 {
		repos.ForeignAnswer = foreign[0]
	}
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

// The same call proposed twice in a turn, in parallel, is one offer.
func TestProposals_IdenticalCallsShareOneOffer(t *testing.T) {
	h, store, ran := proposalHarness(t)
	home := viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`)
	call := fakemodel.Call{Name: "bulkThing", Input: map[string]any{"n": 5}}
	resp, _, _, err := h.turn(home, "do 5 things",
		fakemodel.Tools(call, call, fakemodel.Call{Name: "bulkThing", Input: map[string]any{"n": 6}}),
		fakemodel.Answer(`{"explanation":"Ready when you are.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if *ran != 0 || len(store.rows) != 2 || len(resp.Proposals) != 2 {
		t.Fatalf("ran = %d, rows = %d, cards = %+v, want 2 offers", *ran, len(store.rows), resp.Proposals)
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
	if err != nil || *ran != 4 || resp.Explanation != "Done: Do 4 things" ||
		!slices.Equal(events, []SSEEventKind{"bulk_progress", SSEEventProposalResolved, SSEEventComplete}) {
		t.Fatalf("resp = %+v, err = %v, ran = %d, events = %v", resp, err, *ran, events)
	}
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "mine", Confirm: true}, nil); !isConflict(err) {
		t.Fatalf("answering twice: %v, want a conflict", err)
	}
	// An offer for a tool this assistant doesn't have is another
	// assistant's, which answers it.
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "gone", Confirm: true}, nil); !errors.Is(err, ErrNotThisAssistant) {
		t.Fatalf("another assistant's offer: %v", err)
	}
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "nope"}, nil); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("missing offer: %v", err)
	}
	// The answer and its result are in the offer's thread.
	if len(h.msgs.msgs) != 2 || h.msgs.msgs[0].Content != "Confirmed: Do 4 things" || !strings.Contains(h.msgs.msgs[1].Content, "Done: Do 4 things") {
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

// In the campaign's own thread, a "yes" to the campaign assistant's offer
// goes to it, and the turn reports it resolved and what it changed.
func TestProposals_ForeignOfferInChat(t *testing.T) {
	var foreignCalls []AnswerRequest
	h, store, _ := proposalHarness(t, func(ctx context.Context, req AnswerRequest) (any, error) {
		foreignCalls = append(foreignCalls, req)
		assistantkit.Touch(ctx, "idea", "")
		return "campaign work done", nil
	})
	store.rows = []*models.AssistantProposal{
		{ID: "theirs", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "runContentPlan", Summary: "Plan the campaign", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	campaign := viewOf(t, `{"version":1,"screen":"campaign.overview","scope":{"kind":"campaign","id":"c1"}}`)
	resp, events, _, err := h.turn(campaign, "yes",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": "theirs"}}),
		fakemodel.Answer(`{"explanation":"Planned.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(foreignCalls) != 1 || foreignCalls[0].ProposalID != "theirs" || !foreignCalls[0].Confirm {
		t.Fatalf("foreign answers = %+v", foreignCalls)
	}
	if !slices.Contains(events, SSEEventProposalResolved) || !slices.Contains(resp.Touched, assistantkit.Touched{Kind: "idea"}) {
		t.Fatalf("events = %v, touched = %v; want the offer resolved and the ideas touched", events, resp.Touched)
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

// Confirmed work that refuses to run is a conflict, not "Done"; the offer is
// recorded as answered so the card and the thread don't wait on it.
func TestProposals_CardConfirmRefusedIsConflict(t *testing.T) {
	h, store, ran := proposalHarness(t)
	ctx := context.WithValue(t.Context(), logging.UserIDKey, "u1")
	store.rows = []*models.AssistantProposal{{
		ID: "gone", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(goneBulk)},
		Summary: "Do 14 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour),
	}}

	var events []SSEEventKind
	resp, err := h.tools.answer(ctx, AnswerRequest{ProposalID: "gone", Confirm: true}, func(k SSEEventKind, _ any) { events = append(events, k) })
	if !isConflict(err) || resp != nil || *ran != 0 {
		t.Fatalf("resp = %+v, err = %v, ran = %d; want a conflict", resp, err, *ran)
	}
	if !slices.Equal(events, []SSEEventKind{SSEEventProposalResolved}) {
		t.Fatalf("events = %v; want only proposal_resolved", events)
	}
	thread, _ := h.msgs.ListRecent(ctx, models.AssistantScopeWorkspace, "", 10)
	if len(thread) != 2 || !strings.Contains(thread[1].Content, unfinishedAnswer) {
		t.Fatalf("thread = %+v", thread)
	}
}

// An offer is answered only from its own thread: a "yes" in another
// campaign's thread or in the workspace leaves it pending and unrun.
func TestProposals_ConfirmFromAnotherThreadIsRefused(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{{
		ID: "c1-offer", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "bulkThing", Args: models.JSONMap{"n": float64(5)},
		Summary: "Do 5 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour),
	}}
	for _, view := range []string{
		`{"version":1,"screen":"campaign.overview","scope":{"kind":"campaign","id":"c2"}}`,
		`{"version":1,"screen":"home","scope":{"kind":"workspace"}}`,
	} {
		if _, _, _, err := h.turn(viewOf(t, view), "yes",
			fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": "c1-offer"}}),
			fakemodel.Answer(`{"explanation":"That offer is gone.","action":"answered"}`)); err != nil {
			t.Fatal(err)
		}
		if *ran != 0 || store.rows[0].Status != models.ProposalPending {
			t.Fatalf("from %s: ran = %d, status = %s; want pending, unrun", view, *ran, store.rows[0].Status)
		}
	}
}
