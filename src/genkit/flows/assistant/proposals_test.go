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
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
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

func (m *memProposals) Replacement(_ context.Context, id string, now time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.Replaces == id && p.Open(now) {
			return p.ID, nil
		}
	}
	return "", nil
}

func (m *memProposals) Reopen(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ID == id && p.Status == models.ProposalConfirmed {
			p.Status, p.Chosen = models.ProposalPending, ""
			return true, nil
		}
	}
	return false, nil
}

func (m *memProposals) Expire(_ context.Context, id string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ID == id && p.Status == models.ProposalPending && !now.Before(p.ExpiresAt) {
			p.Status = models.ProposalExpired
			return true, nil
		}
	}
	return false, nil
}

func (m *memProposals) ListByIDs(_ context.Context, ids []string) ([]models.AssistantProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.AssistantProposal
	for _, p := range m.rows {
		if slices.Contains(ids, p.ID) {
			out = append(out, *p)
		}
	}
	return out, nil
}

type bulkIn struct {
	N int `json:"n"`
}

// bulkOut is how many things bulkThing did, and what it left.
type bulkOut struct {
	Done int `json:"done"`
	assistantkit.Partial
}

// brokenBulk is the count bulkThing fails on; goneBulk the one it refuses
// once confirmed, as work whose subject went away while it waited.
// shrunkBulk is the count that shrinks to 5 while its offer waits;
// halfBulk the one that does all but 6 things; vanishedBulk the one none
// of whose things are left once confirmed.
const (
	brokenBulk   = 13
	goneBulk     = 14
	shrunkBulk   = 15
	halfBulk     = 16
	vanishedBulk = 17
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
					Card: func(_ context.Context, in bulkIn) *assistantkit.Card {
						return &assistantkit.Card{Verb: fmt.Sprintf("Do %d", in.N)}
					},
					Recheck: func(_ context.Context, in bulkIn) (bulkIn, []string, error) {
						switch in.N {
						case shrunkBulk:
							in.N = 5
							return in, []string{"10 of the things were deleted"}, nil
						case vanishedBulk:
							return in, []string{"all the things were deleted"}, assistantkit.Refused("none of those things exist any more")
						}
						return in, nil, nil
					},
				}, func(in bulkIn, out *bulkOut) (bulkIn, []assistantkit.NotMade, bool) {
					if out.Done == in.N {
						return in, nil, false
					}
					return bulkIn{N: in.N - out.Done}, []assistantkit.NotMade{{ID: "rest", Reason: "engine hiccup"}}, true
				}, assistantkit.Wrap(func(ctx context.Context, in bulkIn) (*bulkOut, error) {
					if in.N == brokenBulk {
						return nil, errors.New("bulk engine down")
					}
					if in.N == goneBulk {
						return nil, assistantkit.Refused("those things are gone")
					}
					done := in.N
					if in.N == halfBulk {
						done -= 6
					}
					*ran += done
					assistantkit.Touch(ctx, "thing", "")
					return &bulkOut{Done: done}, nil
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
		resp.Proposals[0].Summary != "Do 5 things" || resp.Proposals[0].Card == nil || resp.Proposals[0].Card.Verb != "Do 5" ||
		!slices.Contains(events, SSEEventProposalCreated) {
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
// confirmed: it resolves stale with what changed and stays open, what still
// holds comes back on a fresh card that replaces it, and only that one can
// be confirmed from then on.
func TestProposals_AnswerFromTheCardReoffersWhatStillHolds(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "stale", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(shrunkBulk)},
			Summary: "Do 15 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	var events []SSEEventKind
	var resolved []ProposalResolved
	resp, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "stale", Confirm: true}, func(k SSEEventKind, data any) {
		events = append(events, k)
		if r, ok := data.(ProposalResolved); ok {
			resolved = append(resolved, r)
		}
	})
	if err != nil || *ran != 0 || resp.Explanation != "Nothing ran: 10 of the things were deleted. What still holds is offered again." {
		t.Fatalf("resp = %+v, err = %v, ran = %d", resp, err, *ran)
	}
	if len(resp.Proposals) != 1 || resp.Proposals[0].Summary != "Do 5 things" || resp.Proposals[0].Replaces != "stale" ||
		len(store.rows) != 2 || store.rows[0].Status != models.ProposalPending || store.rows[1].Replaces != "stale" {
		t.Fatalf("cards = %+v, rows = %+v", resp.Proposals, store.rows)
	}
	fresh := resp.Proposals[0].ID
	want := ProposalResolved{ID: "stale", Status: resolvedStale, Reasons: []string{"10 of the things were deleted"}, ReplacedBy: fresh}
	if len(resolved) != 1 || !slices.Equal(resolved[0].Reasons, want.Reasons) || resolved[0].Status != want.Status || resolved[0].ReplacedBy != fresh ||
		len(resp.Resolved) != 1 || resp.Resolved[0].ReplacedBy != fresh {
		t.Fatalf("resolved = %+v, in the response %+v; want %+v", resolved, resp.Resolved, want)
	}
	if !slices.Equal(events, []SSEEventKind{SSEEventProposalCreated, SSEEventProposalResolved, SSEEventComplete}) {
		t.Fatalf("events = %v", events)
	}
	thread, _ := h.msgs.ListRecent(t.Context(), models.AssistantScopeWorkspace, "", 10)
	if len(thread) != 2 || thread[0].Content != "Stale: Do 15 things" {
		t.Fatalf("thread = %+v", thread)
	}
	// The stale offer stays open, but the fresh one is the one to answer.
	_, err = h.tools.answer(t.Context(), AnswerRequest{ProposalID: "stale", Confirm: true}, nil)
	if ce, ok := errors.AsType[*ProposalConflictError](err); !ok || ce.Code != codeOfferReplaced || *ran != 0 {
		t.Fatalf("confirm the stale one again: err = %v, ran = %d", err, *ran)
	}
	if open, _ := store.ListOpen(t.Context(), models.AssistantScopeWorkspace, "", time.Now()); len(open) != 2 {
		t.Fatalf("open = %d", len(open))
	}
	if _, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: fresh, Confirm: true}, nil); err != nil || *ran != 5 {
		t.Fatalf("fresh: err = %v, ran = %d", err, *ran)
	}
}

// An offer none of whose subject is left runs nothing: it resolves stale
// with what changed, stays open, and the answer is no conflict.
func TestProposals_AnswerFromTheCardNothingHolds(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "vanished", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(vanishedBulk)},
			Summary: "Do 17 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	resp, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "vanished", Confirm: true}, nil)
	if err != nil || *ran != 0 || resp.Explanation != "Nothing ran: all the things were deleted." || len(resp.Proposals) != 0 {
		t.Fatalf("resp = %+v, err = %v, ran = %d", resp, err, *ran)
	}
	if store.rows[0].Status != models.ProposalPending || len(resp.Resolved) != 1 || resp.Resolved[0].Status != resolvedStale || resp.Resolved[0].ReplacedBy != "" {
		t.Fatalf("row = %+v, resolved = %+v", store.rows[0], resp.Resolved)
	}
}

// A yes after the offer's expiry runs nothing: the offer is marked
// expired, resolves so, and the answer is a conflict saying why.
func TestProposals_AnswerFromTheCardExpired(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "old", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(5)},
			Summary: "Do 5 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(-time.Minute)},
	}
	var resolved []ProposalResolved
	_, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "old", Confirm: true}, func(_ SSEEventKind, data any) {
		if r, ok := data.(ProposalResolved); ok {
			resolved = append(resolved, r)
		}
	})
	if ce, ok := errors.AsType[*ProposalConflictError](err); !ok || ce.Code != codeOfferExpired || *ran != 0 {
		t.Fatalf("err = %v, ran = %d; want an expired conflict", err, *ran)
	}
	if store.rows[0].Status != models.ProposalExpired || len(resolved) != 1 || resolved[0].Status != resolvedExpired {
		t.Fatalf("row = %+v, resolved = %+v", store.rows[0], resolved)
	}
	thread, _ := h.msgs.ListRecent(t.Context(), models.AssistantScopeWorkspace, "", 10)
	if len(thread) != 2 || thread[0].Content != "Expired: Do 5 things" {
		t.Fatalf("thread = %+v", thread)
	}
	// Once marked, it stays expired, and isn't announced again.
	resolved = nil
	if _, err := h.tools.answer(t.Context(), AnswerRequest{ProposalID: "old", Confirm: true}, nil); !isConflict(err) || len(resolved) != 0 {
		t.Fatalf("again: err = %v", err)
	}
}

// A typed yes to an offer that changed resolves stale, as the card's does.
func TestProposals_ConfirmInChatStale(t *testing.T) {
	h, store, ran := proposalHarness(t)
	store.rows = []*models.AssistantProposal{
		{ID: "stale", ScopeKind: models.AssistantScopeWorkspace, Tool: "bulkThing", Args: models.JSONMap{"n": float64(shrunkBulk)},
			Summary: "Do 15 things", Status: models.ProposalPending, ExpiresAt: time.Now().Add(time.Hour)},
	}
	home := viewOf(t, `{"version":1,"screen":"home","scope":{"kind":"workspace"}}`)
	resp, _, _, err := h.turn(home, "yes",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": "stale"}}),
		fakemodel.Answer(`{"explanation":"One went; here's the rest.","action":"answered"}`))
	if err != nil || *ran != 0 {
		t.Fatalf("err = %v, ran = %d", err, *ran)
	}
	if len(resp.Resolved) != 1 || resp.Resolved[0].Status != resolvedStale || resp.Resolved[0].ReplacedBy == "" || store.rows[0].Status != models.ProposalPending {
		t.Fatalf("resolved = %+v, row = %+v", resp.Resolved, store.rows[0])
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

// The recommended option is stored with the offer and shown on its card.
func TestProposals_RecommendedOptionOnTheCard(t *testing.T) {
	store := &memProposals{}
	st := newTurnState(&viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "home",
		Scope: viewcontext.Scope{Kind: models.AssistantScopeWorkspace}}}, nil)
	sink := turnSink{st: st, repos: Repos{Proposals: store}}
	_, err := sink.Propose(t.Context(), assistantkit.Proposal{Tool: "bulkThing", Args: bulkIn{N: 9}, Summary: "Over the goal", Options: []assistantkit.Option{
		{ID: "fill", Title: "Add 2", Recommended: true, Tool: "bulkThing", Args: bulkIn{N: 2}},
		{ID: "anyway", Title: "Add all 9", Tool: "bulkThing", Args: bulkIn{N: 9}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !store.rows[0].Options[0].Recommended || store.rows[0].Options[1].Recommended {
		t.Fatalf("stored = %+v", store.rows[0].Options)
	}
	if cards := st.cardList(); !cards[0].Options[0].Recommended || cards[0].Options[1].Recommended {
		t.Fatalf("card = %+v", cards[0].Options)
	}
}
