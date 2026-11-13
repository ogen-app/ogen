package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// sourceWait is how long a request waits for the source its user went to
// add before it is no longer offered again.
const sourceWait = proposalTTL

// EventThreadOffer is the event a thread's fresh offer publishes on its
// campaign's topic, for an open thread to reload its history.
const EventThreadOffer = "assistant.offer.created"

// ThreadOfferPayload names the thread and the offers made in it.
type ThreadOfferPayload struct {
	Scope       viewcontext.Scope `json:"scope"`
	ProposalIDs []string          `json:"proposalIds"`
}

// EventPublisher publishes an event to the clients listening;
// eventhub.Hub.
type EventPublisher interface {
	Publish(ctx context.Context, ev eventhub.Event) error
}

// sourceLocks keeps two sources becoming ready at once from offering a
// campaign's waiting request twice.
var sourceLocks sync.Map

// sourceReady offers again, in a campaign's thread, each request that
// waited for its user to add a source, now that the source named title is
// ready: as a fresh offer replacing the one answered by going to add it,
// with its card, recorded in the thread and announced on the campaign's
// topic. A request that can't be offered now, e.g. refused, keeps waiting.
func (t *toolSet) sourceReady(ctx context.Context, campaignID, title string) error {
	if t.deps.Proposals == nil || t.deps.Messages == nil {
		return nil
	}
	mu, _ := sourceLocks.LoadOrStore(campaignID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	waiting, err := t.deps.Proposals.ListAwaiting(ctx, models.AssistantScopeCampaign, campaignID, assistantkit.AwaitingSource, time.Now().UTC().Add(-sourceWait))
	if err != nil {
		return fmt.Errorf("list offers waiting on a source: %w", err)
	}
	var errs []error
	for _, p := range waiting {
		if err := t.reoffer(ctx, p, title); err != nil {
			errs = append(errs, fmt.Errorf("offer %s again: %w", p.ID, err))
		}
	}
	return errors.Join(errs...)
}

// reoffer offers p's request again as the user who answered it, from the
// campaign's overview.
func (t *toolSet) reoffer(ctx context.Context, p models.AssistantProposal, title string) error {
	scope := viewcontext.Scope{Kind: p.ScopeKind, ID: p.ScopeID}
	view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "campaign.overview", Scope: scope}}
	st := newTurnState(view, nil)
	st.turn.Instruction = "Source ready: “" + title + "”"
	ctx = t.bind(withTurnState(logging.WithUserID(ctx, p.ResolvedBy), st), st)
	ctx, err := t.beginFor(ctx, st, p.Tool, p.Args)
	if _, refused := errors.AsType[*assistantkit.Refusal](err); refused {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := t.reg.OfferAgain(assistantkit.WithOffer(ctx, p.ID), p.Tool, map[string]any(p.Args)); err != nil {
		return err
	}
	cards := st.cardList()
	if len(cards) == 0 {
		return nil
	}
	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = c.ID
	}
	resp := &Response{Action: actionAnswered, Explanation: fmt.Sprintf("“%s” is ready, so this can draw on it now: %s.", title, cards[0].Summary)}
	finish(st, resp)
	// An offer the thread doesn't show would end the wait unseen: without
	// its turn, it is removed, and the request waits for the next source.
	if err := persistTurn(ctx, t.deps, Request{Instruction: st.turn.Instruction, View: view}, resp); err != nil {
		if derr := t.deps.Proposals.Delete(ctx, ids); derr != nil {
			err = errors.Join(err, fmt.Errorf("remove the unshown offer: %w", derr))
		}
		return fmt.Errorf("record the offer: %w", err)
	}
	if t.deps.Events != nil {
		ev := eventhub.Event{Topic: "entity:campaign:" + p.ScopeID, Type: EventThreadOffer, Payload: ThreadOfferPayload{Scope: scope, ProposalIDs: ids}}
		if err := t.deps.Events.Publish(ctx, ev); err != nil {
			slog.WarnContext(ctx, "announce the offer failed", logging.AttrComponent, logComponent, logging.AttrError, err)
		}
	}
	return nil
}
