package campaign_assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// ConfirmProposalInput names the offer the user accepted.
type ConfirmProposalInput struct {
	ProposalID string `json:"proposalId"         jsonschema:"description=Id of the offer under \"Waiting for the user's answer\" that the user accepted."`
	OptionID   string `json:"optionId,omitempty" jsonschema:"description=Id of the option the user picked, when the offer listed options. Omit otherwise."`
}

// DismissProposalInput names the offer the user turned down.
type DismissProposalInput struct {
	ProposalID string `json:"proposalId" jsonschema:"description=Id of the offer under \"Waiting for the user's answer\" that the user turned down."`
}

// DismissProposalOutput confirms the offer was withdrawn.
type DismissProposalOutput struct {
	Dismissed bool `json:"dismissed"`
}

const (
	refuseOfferGone     = "That offer is no longer available. Ask again and I'll set it up."
	refuseOfferAnswered = "That offer has already been answered or has expired. Ask again and I'll set it up."
)

// confirmProposalTool runs an accepted offer. The model gets the proposed
// tool's own outcome, as if it had just called that tool, or a refusal when
// the offer can't be run.
func confirmProposalTool(ctx *ai.ToolContext, in ConfirmProposalInput) (any, error) {
	out, err := toolConfirmProposal(ctx, in)
	if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
		return assistantkit.Refuse[struct{}](r.Reason, r.Alternatives...), nil
	}
	return out, err
}

// toolConfirmProposal resolves the offer first, so a typed "yes" racing the
// card's button runs the work once, then runs the stored call (or the chosen
// option's call) exactly as stored, past its gate.
func toolConfirmProposal(ctx context.Context, in ConfirmProposalInput) (any, error) {
	st := getRequestState(ctx)
	p, err := loadOpenProposal(ctx, st, in.ProposalID)
	if err != nil {
		return nil, err
	}
	tool, args := p.Tool, p.Args
	if in.OptionID != "" {
		o, ok := p.Option(in.OptionID)
		if !ok {
			ids := make([]string, len(p.Options))
			for i, o := range p.Options {
				ids[i] = o.ID
			}
			return nil, assistantkit.Refused(fmt.Sprintf("%q is not one of the options offered.", in.OptionID), ids...)
		}
		if o.Tool != "" {
			tool, args = o.Tool, o.Args
		}
	}
	// What the work needs is claimed before the offer is answered, so an
	// offer whose work can't start now stays open.
	ctx, release, err := st.registry.Prepare(ctx, tool)
	if err != nil {
		return nil, err
	}
	if err := resolveProposal(ctx, st, p.ID, models.ProposalConfirmed, in.OptionID); err != nil {
		release()
		return nil, err
	}
	return st.registry.RunConfirmed(ctx, tool, map[string]any(args))
}

func toolDismissProposal(ctx context.Context, in DismissProposalInput) (*DismissProposalOutput, error) {
	st := getRequestState(ctx)
	p, err := loadOpenProposal(ctx, st, in.ProposalID)
	if err != nil {
		return nil, err
	}
	if err := resolveProposal(ctx, st, p.ID, models.ProposalDismissed, ""); err != nil {
		return nil, err
	}
	return &DismissProposalOutput{Dismissed: true}, nil
}

// loadOpenProposal returns the campaign's offer if it can still be answered.
// Another campaign's offer reads as gone.
func loadOpenProposal(ctx context.Context, st *requestState, id string) (*models.AssistantProposal, error) {
	if st.repos.Proposals == nil {
		return nil, assistantkit.ErrNoProposalSink
	}
	p, err := st.repos.Proposals.GetByID(ctx, strings.TrimSpace(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assistantkit.Refused(refuseOfferGone)
	}
	if err != nil {
		return nil, fmt.Errorf("load proposal: %w", err)
	}
	if p.ScopeKind != models.AssistantScopeCampaign || p.ScopeID != st.campaignID {
		return nil, assistantkit.Refused(refuseOfferGone)
	}
	if !p.Open(time.Now().UTC()) {
		return nil, assistantkit.Refused(refuseOfferAnswered)
	}
	return p, nil
}

// resolveProposal records the answer. Losing the race to another answer is a
// refusal, so the work never runs twice.
func resolveProposal(ctx context.Context, st *requestState, id string, status models.AssistantProposalStatus, chosen string) error {
	by, _ := logging.UserIDFrom(ctx)
	ok, err := st.repos.Proposals.Resolve(ctx, id, status, chosen, by, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("resolve proposal: %w", err)
	}
	if !ok {
		return assistantkit.Refused(refuseOfferAnswered)
	}
	st.mu.Lock()
	st.resolved = append(st.resolved, ProposalResolvedEventPayload{ID: id, Status: string(status), Chosen: chosen})
	st.mu.Unlock()
	return nil
}

// resolvedList is the offers answered this turn so far.
func (st *requestState) resolvedList() []ProposalResolvedEventPayload {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.resolved)
}

// resolvedOutcomes report this turn's answered offers. They label nothing: a
// confirmed offer's work labels the turn through its own outcome, a review
// alongside keeps its label, and a dismissal alone reads as a plain answer.
func resolvedOutcomes(resolved []ProposalResolvedEventPayload) []outcome {
	out := make([]outcome, 0, len(resolved))
	for _, r := range resolved {
		explanation := "Done."
		if r.Status == string(models.ProposalDismissed) {
			explanation = "OK, I've dropped that."
		}
		out = append(out, outcome{
			explanation: explanation,
			event:       SSEEventProposalResolved,
			payload:     r,
			readOnly:    true,
		})
	}
	return out
}
