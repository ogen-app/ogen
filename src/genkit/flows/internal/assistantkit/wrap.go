package assistantkit

import (
	"context"
	"errors"

	"github.com/firebase/genkit/go/ai"
)

// Refusal is an expected failure a tool body reports as an error. Wrap turns
// it into a refused outcome instead of letting it abort the turn.
type Refusal struct {
	Reason       string
	Alternatives []string
	// Code names the refusal for the client's card, e.g. idea_read_only;
	// empty for one the client only shows as prose.
	Code string
}

// Error is the reason, which the model relays to the user.
func (r *Refusal) Error() string { return r.Reason }

// Refused returns a Refusal error for a request the tool cannot do as asked.
func Refused(reason string, alternatives ...string) error {
	return &Refusal{Reason: reason, Alternatives: alternatives}
}

// RefusedCode is Refused with a code the client draws its own card for.
func RefusedCode(code, reason string, alternatives ...string) error {
	return &Refusal{Reason: reason, Alternatives: alternatives, Code: code}
}

// Matcher recognises an error type a dependency uses for user-correctable
// input and returns the reason to show the model.
type Matcher func(error) (reason string, ok bool)

// As matches errors of type E anywhere in the chain.
func As[E error](reason func(E) string) Matcher {
	return func(err error) (string, bool) {
		if e, ok := errors.AsType[E](err); ok {
			return reason(e), true
		}
		return "", false
	}
}

// Wrap adapts a tool body returning (Out, error) into a genkit tool function
// returning Outcome[Out]. A *Refusal, or an error one of refusable recognises,
// becomes a refused outcome. Any other error is returned unchanged and aborts
// the turn, which is right for infrastructure failures.
func Wrap[In, Out any](body func(context.Context, In) (Out, error), refusable ...Matcher) ai.ToolFunc[In, Outcome[Out]] {
	return func(ctx *ai.ToolContext, in In) (Outcome[Out], error) {
		out, err := body(ctx, in)
		if err == nil {
			return OK(out), nil
		}
		if r, ok := errors.AsType[*Refusal](err); ok {
			return Refuse[Out](r.Reason, r.Alternatives...), nil
		}
		for _, m := range refusable {
			if reason, ok := m(err); ok {
				return Refuse[Out](reason), nil
			}
		}
		return Outcome[Out]{}, err
	}
}
