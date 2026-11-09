package packs

import (
	"context"
	"maps"
	"sync"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
)

// toolResults collects a pack's tools' last outputs in one turn, by tool
// name. A pack's tools run concurrently, so it locks.
type toolResults struct {
	mu  sync.Mutex
	out map[string]any
}

type toolResultsKey struct{ pack string }

// beginResults readies pack's collector for one turn.
func beginResults(pack string) func(ctx context.Context, _ assistant.Turn) (context.Context, string, error) {
	return func(ctx context.Context, _ assistant.Turn) (context.Context, string, error) {
		return context.WithValue(ctx, toolResultsKey{pack}, &toolResults{out: map[string]any{}}), "", nil
	}
}

// endResults reports pack's collected outputs, by tool name; nil when no
// tool gave one.
func endResults(pack string) func(ctx context.Context) (*assistant.PackResult, error) {
	return func(ctx context.Context) (*assistant.PackResult, error) {
		r, ok := ctx.Value(toolResultsKey{pack}).(*toolResults)
		if !ok {
			return nil, nil
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.out) == 0 {
			return nil, nil
		}
		return &assistant.PackResult{Value: maps.Clone(r.out)}, nil
	}
}

// recorded is fn whose output, when it succeeds, is the turn's result for
// tool, as view shapes it (nil: as it is). A refused call records nothing;
// behind a gate it runs only once confirmed, so an offer records nothing
// either.
func recorded[In, Out any](pack, tool string, view func(*Out) any, fn func(context.Context, In) (*Out, error)) func(context.Context, In) (*Out, error) {
	return func(ctx context.Context, in In) (*Out, error) {
		out, err := fn(ctx, in)
		if err != nil || out == nil {
			return out, err
		}
		if r, ok := ctx.Value(toolResultsKey{pack}).(*toolResults); ok {
			var v any = out
			if view != nil {
				v = view(out)
			}
			r.mu.Lock()
			r.out[tool] = v
			r.mu.Unlock()
		}
		return out, nil
	}
}
