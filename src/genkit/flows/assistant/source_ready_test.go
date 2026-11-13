package assistant

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// planIn is planThing's input: how many things to plan.
type planIn struct {
	N int `json:"n"`
}

// planPack has one gated campaign tool that runs a small plan straight away
// and offers a big one; ran counts what it planned.
func planPack(ran *int) Pack {
	return Pack{
		Name: "plans", Summary: "plans", Scopes: []models.AssistantScopeKind{models.AssistantScopeCampaign},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "planThing", "Plans n things.",
				assistantkit.Gated("planThing", assistantkit.Gate[planIn]{
					Needs:   func(_ context.Context, in planIn) bool { return in.N > 3 },
					Summary: func(in planIn) string { return fmt.Sprintf("Plan %d things", in.N) },
				}, assistantkit.Wrap(func(_ context.Context, in planIn) (int, error) {
					*ran += in.N
					return in.N, nil
				})))
			return []assistantkit.Spec{{Name: tool.Name(), Pack: "plans", Scopes: allScopes, Tier: assistantkit.TierProposal, Guide: "Plans n things.", Tool: tool}}, nil
		},
	}
}

// memEvents keeps what was published.
type memEvents struct {
	mu  sync.Mutex
	got []eventhub.Event
}

func (e *memEvents) Publish(_ context.Context, ev eventhub.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
	return nil
}

// A request that waited for its user to add a source comes back, once one
// is ready, as a fresh offer replacing the answered one, even a small call
// that would have run, made for whoever answered it: recorded in the
// thread and announced on the campaign. Nothing runs, it comes back once,
// and an offer answered otherwise, or too long ago, stays as it is.
func TestSourceReady_OffersTheWaitingRequestAgain(t *testing.T) {
	ran := new(int)
	store := &memProposals{}
	h := proposalsHarness(t, store, planPack(ran))
	events := &memEvents{}
	h.tools.deps.Events = events
	now := time.Now().UTC()
	waited := func(id, chosen string, at time.Time) *models.AssistantProposal {
		return &models.AssistantProposal{ID: id, ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "planThing", Args: models.JSONMap{"n": float64(2)},
			Summary: "No sources", Status: models.ProposalConfirmed, Chosen: chosen, ResolvedBy: "u1", ResolvedAt: &at, ExpiresAt: now.Add(time.Hour)}
	}
	store.rows = []*models.AssistantProposal{
		waited("wait", assistantkit.OptionAddDocument, now),
		waited("went-ahead", assistantkit.OptionExploratory, now),
		waited("long-ago", assistantkit.OptionImportPage, now.Add(-2*sourceWait)),
	}

	if err := h.tools.sourceReady(t.Context(), "c1", "Brochure"); err != nil {
		t.Fatal(err)
	}
	if *ran != 0 || len(store.rows) != 4 {
		t.Fatalf("ran = %d, rows = %d", *ran, len(store.rows))
	}
	fresh := store.rows[3]
	if fresh.Replaces != "wait" || fresh.Tool != "planThing" || fresh.Status != models.ProposalPending || fresh.Summary != "Plan 2 things" ||
		fresh.CreatedBy != "u1" || fresh.ScopeID != "c1" {
		t.Fatalf("fresh = %+v", fresh)
	}
	if len(h.msgs.msgs) != 2 || h.msgs.msgs[0].Content != "Source ready: “Brochure”" ||
		!strings.Contains(h.msgs.msgs[1].Content, `“Brochure” is ready`) || !strings.Contains(h.msgs.msgs[1].Content, fresh.ID) {
		t.Fatalf("thread = %+v", h.msgs.msgs)
	}
	if len(events.got) != 1 || events.got[0].Topic != "entity:campaign:c1" || events.got[0].Type != EventThreadOffer {
		t.Fatalf("events = %+v", events.got)
	}

	// Another source becoming ready finds nothing waiting.
	if err := h.tools.sourceReady(t.Context(), "c1", "Menu"); err != nil || len(store.rows) != 4 || len(events.got) != 1 {
		t.Fatalf("again: err = %v, rows = %d, events = %d", err, len(store.rows), len(events.got))
	}
}

// The fresh offer is answered like any other: confirming it runs the
// request.
func TestSourceReady_FreshOfferRuns(t *testing.T) {
	ran := new(int)
	store := &memProposals{}
	h := proposalsHarness(t, store, planPack(ran))
	at := time.Now().UTC()
	store.rows = []*models.AssistantProposal{{ID: "wait", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "planThing", Args: models.JSONMap{"n": float64(2)},
		Status: models.ProposalConfirmed, Chosen: assistantkit.OptionImportPage, ResolvedBy: "u1", ResolvedAt: &at, ExpiresAt: at.Add(time.Hour)}}
	if err := h.tools.sourceReady(t.Context(), "c1", "Spa page"); err != nil {
		t.Fatal(err)
	}
	ctx := logging.WithUserID(t.Context(), "u1")
	if _, err := h.tools.answer(ctx, AnswerRequest{ProposalID: store.rows[1].ID, Confirm: true}, nil); err != nil || *ran != 2 {
		t.Fatalf("err = %v, ran = %d", err, *ran)
	}
}
