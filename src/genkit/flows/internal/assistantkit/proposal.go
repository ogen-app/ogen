package assistantkit

import (
	"context"
	"errors"

	"github.com/firebase/genkit/go/ai"
)

// Proposal is a tool call a gate held back for the user to confirm.
type Proposal struct {
	Tool string
	// Args is the tool input exactly as the model sent it; confirming runs
	// the tool with it unchanged.
	Args any
	// Summary describes the call on the proposal card.
	Summary string
	Options []Option
}

// ProposalSink stores held-back calls. The runner puts one in the context of
// every turn.
type ProposalSink interface {
	// Propose stores p as an offer waiting for the user's answer and returns
	// its id, which confirming or dismissing it names.
	Propose(ctx context.Context, p Proposal) (id string, err error)
}

// ErrNoProposalSink is returned when a gated tool runs without a sink in its
// context: the call can neither run unconfirmed nor be stored.
var ErrNoProposalSink = errors.New("assistantkit: proposal-tier tool called without a proposal sink")

type sinkKey struct{}

type confirmedKey struct{}

// WithProposalSink returns ctx carrying the sink gated tools store into.
func WithProposalSink(ctx context.Context, s ProposalSink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

// WithConfirmed marks ctx as running a call the user already confirmed, so
// gates let it through instead of proposing it again.
func WithConfirmed(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmedKey{}, true)
}

// Confirmed reports whether ctx runs a confirmed call.
func Confirmed(ctx context.Context) bool {
	v, _ := ctx.Value(confirmedKey{}).(bool)
	return v
}

// Gate decides when a proposal-tier tool must wait for the user's yes.
type Gate[In any] struct {
	// Precheck validates the call before anything is proposed, so a request
	// that could never run is refused now instead of after the user said yes.
	// A *Refusal becomes a refused outcome; any other error aborts the turn.
	Precheck func(context.Context, In) error
	// Needs reports whether this call must be confirmed first. Nil means
	// every call must, e.g. regenerating a whole content plan. A count-based
	// tool returns true only above its threshold, so a small request runs
	// straight away.
	Needs func(context.Context, In) bool
	// Summary describes the call on the proposal card, e.g. "Draft 5 LinkedIn
	// posts for the launch phase".
	Summary func(In) string
}

// Gated holds a proposal-tier tool back until the user confirms it. Unless
// the call is confirmed or below the gate's threshold, it is stored through
// the context's sink and the model gets a proposed outcome to relay instead of
// the tool running. A confirmed call runs next unchanged.
func Gated[In, Out any](tool string, gate Gate[In], next ai.ToolFunc[In, Outcome[Out]]) ai.ToolFunc[In, Outcome[Out]] {
	return func(ctx *ai.ToolContext, in In) (Outcome[Out], error) {
		if Confirmed(ctx) {
			return next(ctx, in)
		}
		if gate.Precheck != nil {
			if err := gate.Precheck(ctx, in); err != nil {
				if r, ok := errors.AsType[*Refusal](err); ok {
					return Refuse[Out](r.Reason, r.Alternatives...), nil
				}
				return Outcome[Out]{}, err
			}
		}
		if gate.Needs != nil && !gate.Needs(ctx, in) {
			return next(ctx, in)
		}
		sink, _ := ctx.Value(sinkKey{}).(ProposalSink)
		if sink == nil {
			return Outcome[Out]{}, ErrNoProposalSink
		}
		summary := tool
		if gate.Summary != nil {
			summary = gate.Summary(in)
		}
		id, err := sink.Propose(ctx, Proposal{Tool: tool, Args: in, Summary: summary})
		if err != nil {
			return Outcome[Out]{}, err
		}
		return Outcome[Out]{Status: StatusProposed, ProposalID: id, Summary: summary}, nil
	}
}
