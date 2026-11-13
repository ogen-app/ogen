package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// proposalTTL is how long an offer waits for an answer.
const proposalTTL = 24 * time.Hour

const (
	refuseOfferGone     = "That offer is no longer available. Ask again and I'll set it up."
	refuseOfferAnswered = "That offer has already been answered. Ask again and I'll set it up."
	refuseOfferExpired  = "That offer lapsed: nothing ran. Ask again and I'll set it up from things as they are now."
	refuseOfferReplaced = "Things changed since that offer, and what still holds was offered afresh: answer that offer instead."
	// refuseOfferUnanswered is a confirmation of an offer made in the same
	// message, before the user saw it.
	refuseOfferUnanswered = "That offer was made in this message and the user hasn't answered it yet. Tell them what it will do and wait for their answer."
)

// ErrProposalNotFound is an offer that doesn't exist for the caller (404).
var ErrProposalNotFound = errors.New("proposal not found")

// ProposalConflictError is an offer that can no longer be answered (409).
// Code names why for the client: offer_expired, offer_answered or
// offer_replaced; empty for the work's own refusal.
type ProposalConflictError struct {
	Msg  string
	Code string
}

func (e *ProposalConflictError) Error() string { return e.Msg }

// ProposalCard is an offer waiting for the user, as its card shows it.
type ProposalCard struct {
	ID        string           `json:"id"`
	Tool      string           `json:"tool"`
	Summary   string           `json:"summary"`
	ExpiresAt string           `json:"expiresAt"`
	Options   []ProposalChoice `json:"options,omitempty"`
	// Card is the card's details: the confirm button's verb, what the work
	// draws on and makes. Absent, the card shows the summary and Confirm.
	Card *assistantkit.Card `json:"card,omitempty"`
	// Replaces is the confirmed offer this one was made afresh for, because
	// what that one was made on changed: the card is what still holds.
	Replaces string `json:"replaces,omitempty"`
	// RetryOf is the confirmed offer whose run left the items this one
	// tries again.
	RetryOf string `json:"retryOf,omitempty"`
}

// ProposalChoice is one option on a card.
type ProposalChoice struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Recommended is the option the card leads with; at most one is.
	Recommended bool `json:"recommended,omitempty"`
}

// ProposalResolved is an offer answered this turn, or found changed or
// expired when the user said yes.
type ProposalResolved struct {
	ID string `json:"id"`
	// Status is confirmed or dismissed; stale when a yes ran nothing
	// because what the offer was made on changed, the offer staying open;
	// expired when the yes came after its expiry.
	Status string `json:"status" enums:"confirmed,dismissed,stale,expired"`
	Chosen string `json:"chosen,omitempty"`
	// Reasons are what changed, for a stale offer.
	Reasons []string `json:"reasons,omitempty"`
	// ReplacedBy is the fresh offer of what still holds, for a stale offer
	// that has one.
	ReplacedBy string `json:"replacedBy,omitempty"`
}

// The resolved statuses that are no answer the user gave.
const (
	resolvedStale   = "stale"
	resolvedExpired = string(models.ProposalExpired)
)

// The codes of a yes refused because the offer can't be answered.
const (
	codeOfferExpired  = "offer_expired"
	codeOfferAnswered = "offer_answered"
	codeOfferReplaced = "offer_replaced"
)

// turnSink stores the turn's held-back calls as the thread's offers.
type turnSink struct {
	st    *turnState
	repos Repos
}

// Propose stores p as an offer in the turn's scope and returns its id. A call
// identical to one already proposed this turn gets that offer's id, so the
// user sees one card for it.
func (s turnSink) Propose(ctx context.Context, p assistantkit.Proposal) (string, error) {
	if s.repos.Proposals == nil {
		return "", assistantkit.ErrNoProposalSink
	}
	args, err := toJSONMap(p.Args)
	if err != nil {
		return "", fmt.Errorf("encode %s proposal args: %w", p.Tool, err)
	}
	// Map keys marshal sorted, so equal args give equal keys.
	key, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode %s proposal args: %w", p.Tool, err)
	}
	dedupKey := p.Tool + "\x00" + string(key)
	// Held across the store so parallel identical calls can't both create one.
	s.st.proposing.Lock()
	defer s.st.proposing.Unlock()
	if id, ok := s.st.proposedKeys[dedupKey]; ok {
		return id, nil
	}
	stored := make([]models.ProposalOption, len(p.Options))
	choices := make([]ProposalChoice, len(p.Options))
	for i, o := range p.Options {
		oargs, err := toJSONMap(o.Args)
		if err != nil {
			return "", fmt.Errorf("encode %s proposal option: %w", p.Tool, err)
		}
		stored[i] = models.ProposalOption{ID: o.ID, Title: o.Title, Description: o.Description, Recommended: o.Recommended, Tool: o.Tool, Args: oargs}
		choices[i] = ProposalChoice{ID: o.ID, Title: o.Title, Description: o.Description, Recommended: o.Recommended}
	}
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	by, _ := logging.UserIDFrom(ctx)
	scope := s.st.view.View.Scope
	row := &models.AssistantProposal{
		ID: id, ScopeKind: scope.Kind, ScopeID: scope.ID, Tool: p.Tool, Args: args, Summary: p.Summary,
		Options: stored, Replaces: p.Replaces, RetryOf: p.RetryOf, CreatedBy: by, ExpiresAt: time.Now().UTC().Add(proposalTTL),
	}
	if err := s.repos.Proposals.Create(ctx, row); err != nil {
		return "", fmt.Errorf("store %s proposal: %w", p.Tool, err)
	}
	s.st.addCard(ProposalCard{
		ID: id, Tool: p.Tool, Summary: p.Summary, ExpiresAt: row.ExpiresAt.Format(time.RFC3339), Options: choices, Card: p.Card,
		Replaces: p.Replaces, RetryOf: p.RetryOf,
	})
	if s.st.proposedKeys == nil {
		s.st.proposedKeys = map[string]string{}
	}
	s.st.proposedKeys[dedupKey] = id
	return id, nil
}

func toJSONMap(v any) (models.JSONMap, error) {
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

// AnswerProposalInput answers an offer waiting in this thread.
type AnswerProposalInput struct {
	ProposalID string `json:"proposalId"         jsonschema:"description=The offer's id, from the offers waiting for an answer"`
	OptionID   string `json:"optionId,omitempty" jsonschema:"description=The option the user picked, when the offer has options"`
}

// DismissProposalOutput confirms a dismissal.
type DismissProposalOutput struct {
	Dismissed bool `json:"dismissed"`
}

// confirmProposal runs a waiting offer exactly as it was stored, past its
// gate, once.
func (t *toolSet) confirmProposal(ctx context.Context, in AnswerProposalInput) (any, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := t.openProposal(ctx, st, in.ProposalID)
	if err != nil {
		return nil, err
	}
	// An offer made in this message waits for the user's answer in the next
	// one: the model can't take its own offer's yes.
	if st.offeredNow(p.ID) {
		return nil, assistantkit.Refused(refuseOfferUnanswered)
	}
	// An offer for a tool no pack has any more can only be dismissed.
	if !t.Owns(p.Tool) && !t.ownsOption(p) {
		return nil, assistantkit.Refused(refuseOfferGone)
	}
	if err := t.notReplaced(ctx, p.ID); err != nil {
		return nil, err
	}
	tool, args, optionID, err := pick(p, in.OptionID)
	if err != nil {
		return nil, err
	}
	in.OptionID = optionID
	// Confirmed work that spends is held to the spend cap; a schedule,
	// which makes no model call, is not. The offer stays open.
	if s, ok := t.reg.Lookup(tool); st.enforce != nil && (!ok || !s.Free) {
		if err := st.enforce(ctx); err != nil {
			return nil, err
		}
	}
	// The pack begins before the offer is resolved, so an offer whose
	// subject is gone stays open rather than answered and not run.
	ctx, err = t.beginFor(ctx, st, tool, args)
	if err != nil {
		return nil, err
	}
	// What the work needs (the campaign's heavy slot) is claimed before the
	// offer is answered too, so work that can't start now leaves it open.
	ctx, release, err := t.reg.Prepare(ctx, tool)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := t.resolve(ctx, st, p.ID, models.ProposalConfirmed, in.OptionID); err != nil {
		return nil, err
	}
	result, err := t.reg.RunConfirmed(assistantkit.WithOffer(ctx, p.ID), tool, map[string]any(args))
	if err != nil {
		return result, err
	}
	// The offer was answered to claim it; a run that found it changed did
	// nothing, so it is open again, and says what changed.
	if changes, replacedBy, ok := staleResult(result); ok {
		if _, err := t.deps.Proposals.Reopen(ctx, p.ID); err != nil {
			return nil, fmt.Errorf("reopen proposal: %w", err)
		}
		st.markStale(p.ID, changes, replacedBy)
	}
	return result, nil
}

// notReplaced refuses an offer that ran nothing because things changed, once
// what still holds was offered afresh: that one is the one to answer.
func (t *toolSet) notReplaced(ctx context.Context, id string) error {
	fresh, err := t.deps.Proposals.Replacement(ctx, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("load replacement: %w", err)
	}
	if fresh != "" {
		return assistantkit.RefusedCode(codeOfferReplaced, refuseOfferReplaced, fresh)
	}
	return nil
}

// pick is the call confirming p with optionID runs, and the option it
// picks: an offer's only option is picked without naming it.
func pick(p *models.AssistantProposal, optionID string) (string, models.JSONMap, string, error) {
	if optionID == "" && len(p.Options) == 1 {
		optionID = p.Options[0].ID
	}
	if optionID == "" && len(p.Options) > 1 {
		return "", nil, "", assistantkit.Refused("This offer has options: pick one.", optionIDs(p)...)
	}
	if optionID == "" {
		return p.Tool, p.Args, "", nil
	}
	o, ok := p.Option(optionID)
	if !ok {
		return "", nil, "", assistantkit.Refused(fmt.Sprintf("%q is not one of the options offered.", optionID), optionIDs(p)...)
	}
	if o.Tool != "" {
		return o.Tool, o.Args, optionID, nil
	}
	return p.Tool, p.Args, optionID, nil
}

func (t *toolSet) dismissProposal(ctx context.Context, in AnswerProposalInput) (*DismissProposalOutput, error) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := t.openProposal(ctx, st, in.ProposalID)
	if err != nil {
		return nil, err
	}
	if err := t.resolve(ctx, st, p.ID, models.ProposalDismissed, ""); err != nil {
		return nil, err
	}
	return &DismissProposalOutput{Dismissed: true}, nil
}

// openProposal returns the thread's offer if it can still be answered.
// Another thread's offer reads as gone.
func (t *toolSet) openProposal(ctx context.Context, st *turnState, id string) (*models.AssistantProposal, error) {
	if t.deps.Proposals == nil {
		return nil, assistantkit.ErrNoProposalSink
	}
	p, err := t.deps.Proposals.GetByID(ctx, strings.TrimSpace(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assistantkit.Refused(refuseOfferGone)
	}
	if err != nil {
		return nil, fmt.Errorf("load proposal: %w", err)
	}
	scope := st.view.View.Scope
	if p.ScopeKind != scope.Kind || p.ScopeID != scope.ID {
		return nil, assistantkit.Refused(refuseOfferGone)
	}
	now := time.Now().UTC()
	if p.Status == models.ProposalPending && !now.Before(p.ExpiresAt) {
		ok, err := t.deps.Proposals.Expire(ctx, p.ID, now)
		if err != nil {
			return nil, fmt.Errorf("expire proposal: %w", err)
		}
		if ok {
			st.addResolved(ProposalResolved{ID: p.ID, Status: resolvedExpired})
		}
		return nil, assistantkit.RefusedCode(codeOfferExpired, refuseOfferExpired)
	}
	switch p.Status {
	case models.ProposalPending:
		return p, nil
	case models.ProposalExpired:
		return nil, assistantkit.RefusedCode(codeOfferExpired, refuseOfferExpired)
	}
	return nil, assistantkit.RefusedCode(codeOfferAnswered, refuseOfferAnswered)
}

// resolve records the answer; losing the race to another answer is a
// refusal, so the work never runs twice.
func (t *toolSet) resolve(ctx context.Context, st *turnState, id string, status models.AssistantProposalStatus, chosen string) error {
	by, _ := logging.UserIDFrom(ctx)
	ok, err := t.deps.Proposals.Resolve(ctx, id, status, chosen, by, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("resolve proposal: %w", err)
	}
	if !ok {
		return assistantkit.RefusedCode(codeOfferAnswered, refuseOfferAnswered)
	}
	st.addResolved(ProposalResolved{ID: id, Status: string(status), Chosen: chosen})
	return nil
}

func optionIDs(p *models.AssistantProposal) []string {
	ids := make([]string, len(p.Options))
	for i, o := range p.Options {
		ids[i] = o.ID
	}
	return ids
}

// openProposals lists the thread's offers still waiting, for the <view>
// block. A failed read lists none: the model then can't confirm by id, which
// the card's button still can.
func (t *toolSet) openProposals(ctx context.Context, scope viewcontext.Scope) []models.AssistantProposal {
	if t.deps.Proposals == nil {
		return nil
	}
	ps, err := t.deps.Proposals.ListOpen(ctx, scope.Kind, scope.ID, time.Now().UTC())
	if err != nil {
		return nil
	}
	return ps
}

// AnswerRequest confirms or dismisses an offer from its card.
type AnswerRequest struct {
	ProposalID string
	OptionID   string
	Confirm    bool
}

// Owns reports whether this assistant made an offer for tool.
func (t *toolSet) Owns(tool string) bool {
	_, ok := t.reg.Lookup(tool)
	return ok
}

// answer confirms or dismisses an offer from its card, without a model call:
// the same work as a "yes" or "no" in the chat.
func (t *toolSet) answer(ctx context.Context, req AnswerRequest, onEvent OnEventFunc) (*Response, error) {
	if t.deps.Proposals == nil {
		return nil, ErrProposalNotFound
	}
	p, err := t.deps.Proposals.GetByID(ctx, req.ProposalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProposalNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load proposal: %w", err)
	}
	view := &viewresolve.Resolved{View: &viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "home", Scope: viewcontext.Scope{Kind: p.ScopeKind, ID: p.ScopeID},
	}}
	st := newTurnState(view, nil)
	st.emit = onEvent
	st.enforce = t.enforce
	st.turn.Instruction = "Confirmed: " + p.Summary
	if !req.Confirm {
		st.turn.Instruction = "Dismissed: " + p.Summary
	}
	ctx = t.bind(withTurnState(ctx, st), st)

	in := AnswerProposalInput{ProposalID: p.ID, OptionID: req.OptionID}
	var result any
	if req.Confirm {
		result, err = t.confirmProposal(ctx, in)
	} else {
		_, err = t.dismissProposal(ctx, in)
	}
	// The thread records a yes that ran nothing as what it found.
	switch st.resolvedAs(p.ID) {
	case resolvedStale:
		st.turn.Instruction = "Stale: " + p.Summary
	case resolvedExpired:
		st.turn.Instruction = "Expired: " + p.Summary
	}
	_, _, stale := staleResult(result)
	reason, refused := refusedResult(result)
	refused = refused && !stale
	switch {
	case err != nil:
		t.recordUnfinished(ctx, st, onEvent, unfinishedAnswer)
	case refused:
		t.recordUnfinished(ctx, st, onEvent, reason)
	}
	if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
		return nil, &ProposalConflictError{Msg: r.Reason, Code: r.Code}
	}
	if err != nil {
		return nil, err
	}
	if refused {
		return nil, &ProposalConflictError{Msg: reason}
	}
	resp := &Response{Action: actionAnswered, Explanation: answered(p, req.Confirm, result), Result: result}
	t.finish(st, resp)
	// The answer joins the thread, so the next chat turn knows it happened.
	// The work has run, so a failed write marks the reply unsaved rather
	// than failing it.
	if t.deps.Messages != nil {
		if err := persistTurn(ctx, t.deps, Request{Instruction: st.turn.Instruction, View: view}, resp); err != nil {
			slog.ErrorContext(ctx, "persist offer answer failed", logging.AttrComponent, logComponent, logging.AttrError, err)
			resp.Unsaved = true
		}
	}
	emitFinal(onEvent, st, resp)
	return resp, nil
}

// answered is the reply to an offer answered from its card, with what its
// work returned.
func answered(p *models.AssistantProposal, confirmed bool, result any) string {
	if !confirmed {
		return "Dismissed: " + p.Summary
	}
	// What the offer was made on changed while it waited: nothing ran, the
	// offer stays open, and what still holds may wait on a fresh card.
	if changes, replacedBy, ok := staleResult(result); ok {
		out := "Nothing ran: " + strings.Join(changes, "; ") + "."
		if replacedBy != "" {
			out += " What still holds is offered again."
		}
		return out
	}
	if left, retry := partialResult(result); left > 0 {
		out := fmt.Sprintf("Done in part: %s. %d not done", p.Summary, left)
		if retry {
			out += "; the ones worth retrying are offered again"
		}
		return out + "."
	}
	return "Done: " + p.Summary
}

// unfinishedAnswer is the reply recorded when an offer was answered but its
// work did not finish.
const unfinishedAnswer = "I couldn't finish that. Ask again and I'll set it up."

// recordUnfinished keeps an offer that was answered before its work failed or
// was refused in the thread, with what the work changed before it stopped,
// and tells the client it was answered, so neither the next turn nor the card
// waits on it. The offer stays answered: work that failed part way must not
// run twice from the same card.
func (t *toolSet) recordUnfinished(ctx context.Context, st *turnState, onEvent OnEventFunc, explanation string) {
	resolved := st.resolvedList()
	if len(resolved) == 0 {
		return
	}
	if t.deps.Messages != nil {
		resp := &Response{Action: actionAnswered, Explanation: explanation, Touched: st.touches.List(), Resolved: resolved}
		if err := persistTurn(ctx, t.deps, Request{Instruction: st.turn.Instruction, View: st.view}, resp); err != nil {
			slog.ErrorContext(ctx, "persist offer answer failed", logging.AttrComponent, logComponent, logging.AttrError, err)
		}
	}
	for _, r := range resolved {
		emit(onEvent, SSEEventProposalResolved, r)
	}
}

// refusedResult reports whether a confirmed tool refused to run — e.g. the
// date it was offered for has passed — with its reason.
func refusedResult(raw any) (string, bool) {
	m, ok := raw.(map[string]any)
	if !ok || m["status"] != string(assistantkit.StatusRefused) {
		return "", false
	}
	reason, _ := m["reason"].(string)
	return reason, true
}

// staleResult reports whether a confirmed tool ran nothing because what the
// offer was made on changed, with what changed and the fresh offer of what
// still holds, when it made one.
func staleResult(raw any) (changes []string, replacedBy string, ok bool) {
	m, _ := raw.(map[string]any)
	list, _ := m["changes"].([]any)
	for _, c := range list {
		if s, ok := c.(string); ok {
			changes = append(changes, s)
		}
	}
	if len(changes) == 0 {
		return nil, "", false
	}
	if m["status"] == string(assistantkit.StatusProposed) {
		replacedBy, _ = m["proposalId"].(string)
	}
	return changes, replacedBy, true
}

// partialResult reports how many items a confirmed run left undone, and
// whether it offered the ones worth retrying; its result says both.
func partialResult(raw any) (left int, retry bool) {
	m, _ := raw.(map[string]any)
	result, ok := m["result"].(map[string]any)
	if !ok {
		return 0, false
	}
	notMade, _ := result["notMade"].([]any)
	id, _ := result["retryProposalId"].(string)
	return len(notMade), id != ""
}

func (t *toolSet) ownsOption(p *models.AssistantProposal) bool {
	for _, o := range p.Options {
		if o.Tool != "" && t.Owns(o.Tool) {
			return true
		}
	}
	return false
}
