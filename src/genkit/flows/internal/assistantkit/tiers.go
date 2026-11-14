package assistantkit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/firebase/genkit/go/ai"
)

// gated holds the names of the tools Gated wraps, so a registry can tell a
// proposal-tier tool that would run unasked.
var gated sync.Map

func isGated(name string) bool {
	_, ok := gated.Load(name)
	return ok
}

// IsGated reports whether a proposal-tier tool of that name is defined, so
// another pack can offer a call to it.
func IsGated(name string) bool { return isGated(name) }

// checkTier reports a tool whose tier and gate disagree: proposal-tier work
// must wait behind a gate, and only it does.
func (s Spec) checkTier() error {
	switch g := isGated(s.Name); {
	case s.Tier == TierProposal && !g:
		return fmt.Errorf("tool %q: a proposal-tier tool must be wrapped in Gated", s.Name)
	case s.Tier != TierProposal && g:
		return fmt.Errorf("tool %q: a gated tool must be proposal tier, not %s", s.Name, s.Tier)
	}
	return nil
}

type gateMarkKey struct{}

// passGate records that a call reached its gate.
func passGate(ctx context.Context) {
	if m, ok := ctx.Value(gateMarkKey{}).(*atomic.Bool); ok {
		m.Store(true)
	}
}

// Guard returns the middleware that holds the registry's tools to their
// tiers at run time: an unconfirmed proposal-tier call runs only through
// its gate, and a call that got past without reaching one is logged.
func (r *Registry) Guard() ai.Middleware { return tierGuard{reg: r} }

type tierGuard struct{ reg *Registry }

// Name identifies the middleware.
func (tierGuard) Name() string { return "assistantkit/tiers" }

// New returns the guard's tool hook for one generation.
func (g tierGuard) New(context.Context) (*ai.Hooks, error) {
	return &ai.Hooks{WrapTool: g.wrapTool}, nil
}

// refuseUngated is what the model gets for a proposal-tier tool with no gate.
const refuseUngated = "This change needs the user's confirmation and can't be offered right now."

func (g tierGuard) wrapTool(ctx context.Context, p *ai.ToolParams, next ai.ToolNext) (*ai.MultipartToolResponse, error) {
	s, ok := g.reg.Lookup(p.Request.Name)
	if !ok || s.Tier != TierProposal || Confirmed(ctx) {
		return next(ctx, p)
	}
	if !isGated(s.Name) {
		slog.ErrorContext(ctx, "proposal-tier tool has no gate; refused", "tool", s.Name)
		return &ai.MultipartToolResponse{Output: Refuse[struct{}](refuseUngated)}, nil
	}
	mark := new(atomic.Bool)
	resp, err := next(context.WithValue(ctx, gateMarkKey{}, mark), p)
	if err == nil && !mark.Load() {
		slog.ErrorContext(ctx, "proposal-tier tool ran without reaching its gate", "tool", s.Name)
	}
	return resp, err
}
