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
	refuseOfferAnswered = "That offer has already been answered or has expired. Ask again and I'll set it up."
	// refuseOfferUnanswered is a confirmation of an offer made in the same
	// message, before the user saw it.
	refuseOfferUnanswered = "That offer was made in this message and the user hasn't answered it yet. Tell them what it will do and wait for their answer."
)

// ErrProposalNotFound is an offer that doesn't exist for the caller (404).
var ErrProposalNotFound = errors.New("proposal not found")

// ProposalConflictError is an offer that can no longer be answered (409).
type ProposalConflictError struct{ Msg string }

func (e *ProposalConflictError) Error() string { return e.Msg }

// ProposalCard is an offer waiting for the user, as its card shows it.
type ProposalCard struct {
	ID        string           `json:"id"`
	Tool      string           `json:"tool"`
	Summary   string           `json:"summary"`
	ExpiresAt string           `json:"expiresAt"`
	Options   []ProposalChoice `json:"options,omitempty"`
}

// ProposalChoice is one option on a card.
type ProposalChoice struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// ProposalResolved is an offer answered this turn.
type ProposalResolved struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Chosen string `json:"chosen,omitempty"`
}

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
		stored[i] = models.ProposalOption{ID: o.ID, Title: o.Title, Description: o.Description, Tool: o.Tool, Args: oargs}
		choices[i] = ProposalChoice{ID: o.ID, Title: o.Title, Description: o.Description}
	}
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	by, _ := logging.UserIDFrom(ctx)
	scope := s.st.view.View.Scope
	row := &models.AssistantProposal{
		ID: id, ScopeKind: scope.Kind, ScopeID: scope.ID, Tool: p.Tool, Args: args, Summary: p.Summary,
		Options: stored, CreatedBy: by, ExpiresAt: time.Now().UTC().Add(proposalTTL),
	}
	if err := s.repos.Proposals.Create(ctx, row); err != nil {
		return "", fmt.Errorf("store %s proposal: %w", p.Tool, err)
	}
	s.st.addCard(ProposalCard{ID: id, Tool: p.Tool, Summary: p.Summary, ExpiresAt: row.ExpiresAt.Format(time.RFC3339), Options: choices})
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
	tool, args := p.Tool, p.Args
	if in.OptionID == "" && len(p.Options) == 1 {
		in.OptionID = p.Options[0].ID
	}
	if in.OptionID == "" && len(p.Options) > 1 {
		return nil, assistantkit.Refused("This offer has options: pick one.", optionIDs(p)...)
	}
	if in.OptionID != "" {
		o, ok := p.Option(in.OptionID)
		if !ok {
			return nil, assistantkit.Refused(fmt.Sprintf("%q is not one of the options offered.", in.OptionID), optionIDs(p)...)
		}
		if o.Tool != "" {
			tool, args = o.Tool, o.Args
		}
	}
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
	if err := t.resolve(ctx, st, p.ID, models.ProposalConfirmed, in.OptionID); err != nil {
		release()
		return nil, err
	}
	return t.reg.RunConfirmed(ctx, tool, map[string]any(args))
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
	if !p.Open(time.Now().UTC()) {
		return nil, assistantkit.Refused(refuseOfferAnswered)
	}
	return p, nil
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
		return assistantkit.Refused(refuseOfferAnswered)
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
	reason, refused := refusedResult(result)
	if err != nil || refused {
		t.recordUnfinished(ctx, st, onEvent)
	}
	if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
		return nil, &ProposalConflictError{Msg: r.Reason}
	}
	if err != nil {
		return nil, err
	}
	if refused {
		return nil, &ProposalConflictError{Msg: reason}
	}
	resp := &Response{Action: actionAnswered, Explanation: "Dismissed: " + p.Summary, Result: result}
	if req.Confirm {
		resp.Explanation = "Done: " + p.Summary
	}
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
	for _, r := range st.resolvedList() {
		emit(onEvent, SSEEventProposalResolved, r)
	}
	emit(onEvent, SSEEventComplete, resp)
	return resp, nil
}

// unfinishedAnswer is the reply recorded when an offer was answered but its
// work did not finish.
const unfinishedAnswer = "I couldn't finish that. Ask again and I'll set it up."

// recordUnfinished keeps an offer that was answered before its work failed or
// was refused in the thread, and tells the client it was answered, so neither
// the next turn nor the card waits on it.
func (t *toolSet) recordUnfinished(ctx context.Context, st *turnState, onEvent OnEventFunc) {
	resolved := st.resolvedList()
	if len(resolved) == 0 {
		return
	}
	if t.deps.Messages != nil {
		resp := &Response{Action: actionAnswered, Explanation: unfinishedAnswer}
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

func (t *toolSet) ownsOption(p *models.AssistantProposal) bool {
	for _, o := range p.Options {
		if o.Tool != "" && t.Owns(o.Tool) {
			return true
		}
	}
	return false
}
