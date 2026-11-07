package campaign_assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
)

// AnswerProposalRequest answers an offer from its card instead of the chat.
type AnswerProposalRequest struct {
	ProposalID string
	// OptionID is the option the user picked, when the offer had options.
	OptionID string
	// Confirm runs the offer; false dismisses it.
	Confirm bool
}

// ErrProposalNotFound means the offer does not exist for this tenant, or does
// not belong to a campaign (HTTP 404).
var ErrProposalNotFound = errors.New("proposal not found")

// ProposalConflictError means the offer can't be answered as asked: it was
// already answered or expired, the option isn't offered, or the work itself
// was refused (HTTP 409). Msg is safe to show the user.
type ProposalConflictError struct{ Msg string }

// Error returns the message shown to the user.
func (e *ProposalConflictError) Error() string { return e.Msg }

// answerProposalRunner is set by InitCampaignAssistant.
var answerProposalRunner func(ctx context.Context, req AnswerProposalRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error)

// NewAnswerProposalCallback returns the callback the proposal endpoints use.
func NewAnswerProposalCallback() func(ctx context.Context, req AnswerProposalRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error) {
	return func(ctx context.Context, req AnswerProposalRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error) {
		return answerProposalRunner(ctx, req, onEvent)
	}
}

// answerProposal confirms or dismisses an offer without a model call. It runs
// the same path as the confirmProposal / dismissProposal tools, then records
// the answer in the conversation so the next chat turn knows it happened.
func answerProposal(ctx context.Context, cfg CampaignAssistantFlowConfig, repos CampaignAssistantRepos, tools *toolSet,
	req AnswerProposalRequest, onEvent OnEventFunc,
) (out *CampaignAssistantResponse, retErr error) {
	if repos.Proposals == nil {
		return nil, ErrProposalNotFound
	}
	p, err := repos.Proposals.GetByID(ctx, req.ProposalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProposalNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load proposal: %w", err)
	}
	if p.ScopeKind != models.AssistantScopeCampaign {
		return nil, ErrProposalNotFound
	}
	campaign, err := repos.Campaigns.GetByID(ctx, p.ScopeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProposalNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	if req.Confirm {
		if err := cfg.Checker.Enforce(ctx); err != nil {
			return nil, err
		}
	}
	defer func() {
		publishAssistantFinalised(cfg.Hub, campaign.ID, campaign.CreatedBy, out, retErr)
		// Fires only when the confirmed work generated a plan.
		notifyContentPlanReady(cfg.Notifier, campaign.TenantID, campaign.CreatedBy, campaign.ID, out, retErr)
	}()

	verb := "Dismissed"
	if req.Confirm {
		verb = "Confirmed"
	}
	st := newRequestState(cfg, repos, campaign, fmt.Sprintf("%s: %s", verb, p.Summary), onEvent)
	st.registry = tools.reg
	ctx = st.bind(brandresolve.WithMemo(ctx))

	var raw any
	if req.Confirm {
		raw, err = toolConfirmProposal(ctx, ConfirmProposalInput{ProposalID: p.ID, OptionID: req.OptionID})
	} else {
		_, err = toolDismissProposal(ctx, DismissProposalInput{ProposalID: p.ID})
	}
	reason, refused := refusedResult(raw)
	if err != nil || refused {
		recordUnfinishedAnswer(ctx, repos, st, onEvent)
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

	resp := responseFromOutcomes(st)
	if err := persistTurn(ctx, repos, CampaignAssistantRequest{CampaignID: campaign.ID, Instruction: st.instruction}, resp); err != nil {
		slog.ErrorContext(ctx, "persist proposal answer failed", logging.AttrComponent, logComponent,
			"campaign_id", campaign.ID, logging.AttrError, err)
	}
	for _, o := range st.outcomes() {
		emit(onEvent, o.event, o.payload)
	}
	emit(onEvent, SSEEventComplete, resp)
	return resp, nil
}

// unfinishedAnswer is the reply recorded when an offer was answered but its
// work did not finish.
const unfinishedAnswer = "I couldn't finish that. Ask again and I'll set it up."

// recordUnfinishedAnswer keeps an offer that was answered before its work
// failed or was refused in the conversation, and tells the client it was
// answered, so neither the next turn nor the card waits on it.
func recordUnfinishedAnswer(ctx context.Context, repos CampaignAssistantRepos, st *requestState, onEvent OnEventFunc) {
	st.mu.Lock()
	resolved := slices.Clone(st.resolved)
	st.mu.Unlock()
	if len(resolved) == 0 {
		return
	}
	resp := &CampaignAssistantResponse{Action: actionAnswered, Explanation: unfinishedAnswer}
	if err := persistTurn(ctx, repos, CampaignAssistantRequest{CampaignID: st.campaignID, Instruction: st.instruction}, resp); err != nil {
		slog.ErrorContext(ctx, "persist proposal answer failed", logging.AttrComponent, logComponent,
			"campaign_id", st.campaignID, logging.AttrError, err)
	}
	for _, r := range resolved {
		emit(onEvent, SSEEventProposalResolved, r)
	}
}

// refusedResult reports whether the confirmed tool refused to run — e.g. the
// proposed date is now in the past — with its reason.
func refusedResult(raw any) (string, bool) {
	m, ok := raw.(map[string]any)
	if !ok || m["status"] != string(assistantkit.StatusRefused) {
		return "", false
	}
	reason, _ := m["reason"].(string)
	return reason, true
}

// responseFromOutcomes builds a turn response from the tools' committed
// results alone, as the runner does when the model gave no reply.
func responseFromOutcomes(st *requestState) *CampaignAssistantResponse {
	outs := st.outcomes()
	resp := &CampaignAssistantResponse{Action: turnAction(outs, st.writes)}
	for _, o := range outs {
		if o.attach != nil {
			o.attach(resp)
		}
		if resp.Explanation == "" {
			resp.Explanation = o.explanation
		}
	}
	if resp.Action == "" {
		resp.Action = actionAnswered
	}
	return resp
}
