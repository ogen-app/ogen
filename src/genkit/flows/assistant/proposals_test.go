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
// shrunkBulk is the count that shrinks by one while its offer waits;
// halfBulk the one that does all but 6 things.
const (
	brokenBulk = 13
	goneBulk   = 14
	shrunkBulk = 15
	halfBulk   = 16
)

// bulkPack has one gated tool: more than 3 items wait for a yes.
func bulkPack(ran *int) Pack {
	return Pack{
		Name: "bulk", Summary: "bulk things", Scopes: []models.AssistantScopeKind{models.AssistantScopeWorkspace},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "bulkThing", "Does n things.",
				assistantkit.GatedRetry("bulkThing", assistantkit.Gate[bulkIn]{
					Needs:   func(_ context.Context, in bulkIn) bool { return in.N > 3 },
					Summary: func(in bulkIn) string { return fmt.Sprintf("Do %d things", in.N) },
					Recheck: func(_ context.Context, in bulkIn) (bulkIn, error) {
						if in.N == shrunkBulk {
							in.N--
						}
						return in, nil
					},
				}, func(in bulkIn, done int) (bulkIn, []assistantkit.NotMade, bool) {
					if done == in.N {
						return in, nil, false
					}
					return bulkIn{N: in.N - done}, []assistantkit.NotMade{{ID: "rest", Reason: "engine hiccup"}}, true
				}, assistantkit.Wrap(func(ctx context.Context, in bulkIn) (int, error) {
					if in.N == brokenBulk {
						return 0, errors.New("bulk engine down")
					}
					if in.N == goneBulk {
						return 0, assistantkit.Refused("those things are gone")
					}
					done := in.N
					if in.N == halfBulk {
						done -= 6
					}
					*ran += done
					assistantkit.Touch(ctx, "thing", "")
					return done, nil
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

// The model can't confirm an offer it made in the same message: it waits
// for the user's answer.
func TestProposals_ConfirmInTheOfferingTurnIsRefused(t *testing.T) {
	h, store, ran := proposalHarness(t)
	st := newTurnState(viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`), nil)
	ctx := h.tools.bind(withTurnState(context.WithValue(t.Context(), logging.UserIDKey, "u1"), st), st)
	id, err := assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "bulkThing", Args: bulkIn{N: 5}, Summary: "Do 5 things"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.tools.confirmProposal(ctx, AnswerProposalInput{ProposalID: id})
	if r, ok := errors.AsType[*assistantkit.Refusal](err); !ok || r.Reason != refuseOfferUnanswered {
		t.Fatalf("err = %v", err)
	}
	if *ran != 0 || store.rows[0].Status != models.ProposalPending {
		t.Fatalf("ran = %d, status = %s", *ran, store.rows[0].Status)
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

// Past the spend cap, a card's yes is refused for work that spends and the
// offer stays open; work that makes no model call still runs.
func TestProposals_AnswerFromTheCardPastTheSpendCap(t *testing.T) {
	ran, freeRan := new(int), 0
	free := Pack{
		Name: "free", Summary: "free things", Scopes: []models.AssistantScopeKind{models.AssistantScopeWorkspace},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "freeThing", "Does a free thing.",
				assistantkit.Gated("freeThing", assistantkit.Gate[struct{}]{}, assistantkit.Wrap(func(context.Context, struct{}) (string, error) {
					freeRan++
					return "done", nil
				})))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "free", Scopes: allScopes, Tier: assistantkit.TierProposal, Guide: "Does a free thing.", Tool: tool, Free: true}}, nil
		},
	}
	store := &memProposals{}
	h := proposalsHarness(t, store, bulkPack(ran), free)
	capped := errors.New("spend cap reached")
	h.tools.enforce = func(context.Context) error { return capped }
	future := time.Now().Add(time.Hour)
	store.rows = []*models.AssistantProposal{
		{ID: "spends", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(4)}, Summary: "Do 4 things", Status: models.ProposalPending, ExpiresAt: future},
		{ID: "free", ScopeKind: models.AssistantScopeWorkspace, Tool: "freeThing", Args: models.JSONMap{}, Summary: "Do a free thing", Status: models.ProposalPending, ExpiresAt: future},
	}
	if _, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "spends", Confirm: true}, nil); !errors.Is(err, capped) || *ran != 0 || store.rows[0].Status != models.ProposalPending {
		t.Fatalf("err = %v, ran = %d, status = %s; want the cap, unrun and open", err, *ran, store.rows[0].Status)
	}
	if _, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "free", Confirm: true}, nil); err != nil || freeRan != 1 {
		t.Fatalf("err = %v, ran = %d; want the free offer run", err, freeRan)
	}
	if _, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "spends"}, nil); err != nil || store.rows[0].Status != models.ProposalDismissed {
		t.Fatalf("dismiss: err = %v, status = %s", err, store.rows[0].Status)
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
	// The thread keeps why it didn't run.
	thread, _ := h.msgs.ListRecent(ctx, models.AssistantScopeWorkspace, "", 10)
	if len(thread) != 2 || !strings.Contains(thread[1].Content, "those things are gone") {
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

// An offer whose subject changed while it waited runs nothing once
// confirmed: what still holds comes back on a fresh card.
func TestProposals_AnswerFromTheCardReoffersWhatStillHolds(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "stale", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(shrunkBulk)},
			Summary: "Do 15 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	var events []SSEEventKind
	resp, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "stale", Confirm: true}, func(k SSEEventKind, _ any) { events = append(events, k) })
	if err != nil || *ran != 0 || !strings.HasPrefix(resp.Explanation, assistantkit.ChangedSince) {
		t.Fatalf("resp = %+v, err = %v, ran = %d", resp, err, *ran)
	}
	if len(resp.Proposals) != 1 || resp.Proposals[0].Summary != "Do 14 things" || len(store.rows) != 2 || store.rows[0].Status != models.ProposalConfirmed || store.rows[1].Status != models.ProposalPending {
		t.Fatalf("cards = %+v, rows = %d", resp.Proposals, len(store.rows))
	}
	if !slices.Equal(events, []SSEEventKind{SSEEventProposalCreated, SSEEventProposalResolved, SSEEventComplete}) {
		t.Fatalf("events = %v", events)
	}
}

// Work that did part of the job says so, and offers what's left on a card
// of its own; its own offer stays answered.
func TestProposals_AnswerFromTheCardDoneInPart(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "half", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(halfBulk)},
			Summary: "Do 16 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	var events []SSEEventKind
	resp, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "half", Confirm: true}, func(k SSEEventKind, _ any) { events = append(events, k) })
	if err != nil || *ran != 10 || !strings.HasPrefix(resp.Explanation, "Done in part: Do 16 things. 1 not done") {
		t.Fatalf("resp = %+v, err = %v, ran = %d", resp, err, *ran)
	}
	if len(resp.Proposals) != 1 || resp.Proposals[0].Summary != "Retry: Do 6 things" || store.rows[0].Status != models.ProposalConfirmed {
		t.Fatalf("cards = %+v", resp.Proposals)
	}
	if !slices.Equal(events, []SSEEventKind{SSEEventProposalCreated, SSEEventProposalResolved, SSEEventComplete}) {
		t.Fatalf("events = %v", events)
	}
	// The retry runs just what was left.
	if _, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: resp.Proposals[0].ID, Confirm: true}, nil); err != nil || *ran != 16 {
		t.Fatalf("retry: err = %v, ran = %d", err, *ran)
	}
}
