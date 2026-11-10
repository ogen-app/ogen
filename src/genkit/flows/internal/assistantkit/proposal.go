package assistantkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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

// ConflictError is a precheck's answer when the request clashes with the
// current state but there are ways forward: the gate stores them as a
// proposal with options, and the user picks one.
type ConflictError struct {
	Reason  string
	Options []Option
}

// Error returns the conflict's reason.
func (e *ConflictError) Error() string { return e.Reason }

// Conflicted returns a ConflictError offering options. Each option's Tool and
// Args are what picking it runs.
func Conflicted(reason string, options ...Option) error {
	return &ConflictError{Reason: reason, Options: options}
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
	// Describe is Summary for a card that needs to read something, e.g. the
	// title of the idea a merge keeps. It wins over Summary when set.
	Describe func(context.Context, In) string
	// Recheck runs when the user confirms, after Precheck, and returns the
	// call narrowed to what still holds, e.g. without the ideas deleted
	// while the offer waited. A call that narrowed is offered afresh
	// instead of run; a *Refusal means nothing holds any more. Nil keeps
	// the call as stored.
	Recheck func(context.Context, In) (In, error)
}

// Gated holds a proposal-tier tool back until the user confirms it. Unless
// the call is confirmed or below the gate's threshold, it is stored through
// the context's sink and the model gets a proposed outcome to relay instead of
// the tool running. A confirmed call runs next unchanged.
func Gated[In, Out any](tool string, gate Gate[In], next ai.ToolFunc[In, Outcome[Out]]) ai.ToolFunc[In, Outcome[Out]] {
	gated.Store(tool, true)
	return func(ctx *ai.ToolContext, in In) (Outcome[Out], error) {
		passGate(ctx)
		if Confirmed(ctx) {
			return runConfirmed(ctx, tool, gate, in, next)
		}
		if gate.Precheck != nil {
			err := gate.Precheck(ctx, in)
			if r, ok := errors.AsType[*Refusal](err); ok {
				return Refuse[Out](r.Reason, r.Alternatives...), nil
			}
			if c, ok := errors.AsType[*ConflictError](err); ok {
				return propose[Out](ctx, Proposal{Tool: tool, Args: in, Summary: c.Reason, Options: c.Options}, StatusConflict)
			}
			if err != nil {
				return Outcome[Out]{}, err
			}
		}
		if gate.Needs != nil && !gate.Needs(ctx, in) {
			return next(ctx, in)
		}
		return propose[Out](ctx, Proposal{Tool: tool, Args: in, Summary: gate.summary(ctx, tool, in)}, StatusProposed)
	}
}

// summary describes the call for its card.
func (g Gate[In]) summary(ctx context.Context, tool string, in In) string {
	switch {
	case g.Describe != nil:
		return g.Describe(ctx, in)
	case g.Summary != nil:
		return g.Summary(in)
	}
	return tool
}

// ChangedSince prefixes why a confirmed offer did not run as offered.
const ChangedSince = "Things changed since the offer: "

// runConfirmed runs a call the user confirmed, unless what it was offered on
// changed while it waited: a call its precheck now refuses runs nothing, and
// one that narrowed is offered afresh with what still holds. A conflict the
// precheck raises again was the user's to answer, and they did.
func runConfirmed[In, Out any](ctx *ai.ToolContext, tool string, gate Gate[In], in In, next ai.ToolFunc[In, Outcome[Out]]) (Outcome[Out], error) {
	if gate.Precheck != nil {
		err := gate.Precheck(ctx, in)
		if r, ok := errors.AsType[*Refusal](err); ok {
			return Refuse[Out](ChangedSince+r.Reason, r.Alternatives...), nil
		}
		if _, ok := errors.AsType[*ConflictError](err); err != nil && !ok {
			return Outcome[Out]{}, err
		}
	}
	if gate.Recheck != nil {
		fresh, err := gate.Recheck(ctx, in)
		if r, ok := errors.AsType[*Refusal](err); ok {
			return Refuse[Out](ChangedSince+r.Reason, r.Alternatives...), nil
		}
		if err != nil {
			return Outcome[Out]{}, err
		}
		same, err := sameArgs(fresh, in)
		if err != nil {
			return Outcome[Out]{}, fmt.Errorf("recheck %s: %w", tool, err)
		}
		if !same {
			out, err := propose[Out](ctx, Proposal{Tool: tool, Args: fresh, Summary: gate.summary(ctx, tool, fresh)}, StatusProposed)
			out.Reason = ChangedSince + "nothing ran, and what still holds is offered again."
			return out, err
		}
	}
	return next(ctx, in)
}

// Retry reads a run that did part of the job: what it left undone, and the
// call that tries just the items worth trying again. ok is false when none
// is.
type Retry[In, Out any] func(in In, out Out) (retry In, notMade []NotMade, ok bool)

// GatedRetry is Gated for a tool that can do part of its job: a run that
// left items undone says which, and offers the ones worth retrying on a
// card of their own. The run's own offer stays answered, so the same work
// never runs twice from it.
func GatedRetry[In, Out any](tool string, gate Gate[In], retry Retry[In, Out], next ai.ToolFunc[In, Outcome[Out]]) ai.ToolFunc[In, Outcome[Out]] {
	return Gated(tool, gate, func(ctx *ai.ToolContext, in In) (Outcome[Out], error) {
		out, err := next(ctx, in)
		if err != nil || !out.Succeeded() || out.Result == nil {
			return out, err
		}
		again, notMade, ok := retry(in, *out.Result)
		out.NotMade = notMade
		if !ok {
			return out, nil
		}
		// The work has run either way: an offer that can't be stored loses
		// only the retry.
		id, err := Offer(ctx, Proposal{Tool: tool, Args: again, Summary: "Retry: " + gate.summary(ctx, tool, again)})
		if err != nil {
			slog.WarnContext(ctx, "offer a retry failed", "tool", tool, "err", err)
			return out, nil
		}
		out.RetryProposalID = id
		return out, nil
	})
}

// sameArgs reports whether two calls encode alike.
func sameArgs(a, b any) (bool, error) {
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ja, jb), nil
}

// Offer stores p as an offer from a tool that computed it — e.g. a plan of
// moves the user confirms as previewed — and returns its id. p.Tool is the
// proposal-tier tool confirming it runs, with p.Args.
func Offer(ctx context.Context, p Proposal) (string, error) {
	sink, _ := ctx.Value(sinkKey{}).(ProposalSink)
	if sink == nil {
		return "", ErrNoProposalSink
	}
	return sink.Propose(ctx, p)
}

// propose stores p and tells the model what is waiting: a plain proposal to
// confirm, or a conflict whose options the user picks from.
func propose[Out any](ctx context.Context, p Proposal, status Status) (Outcome[Out], error) {
	sink, _ := ctx.Value(sinkKey{}).(ProposalSink)
	if sink == nil {
		return Outcome[Out]{}, ErrNoProposalSink
	}
	id, err := sink.Propose(ctx, p)
	if err != nil {
		return Outcome[Out]{}, err
	}
	out := Outcome[Out]{Status: status, ProposalID: id, Summary: p.Summary, Options: p.Options}
	if status == StatusConflict {
		out.Reason = p.Summary
	}
	return out, nil
}
