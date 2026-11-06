package assistantkit

import (
	"context"
	"slices"
	"sync"
)

// Touched names what a turn changed, so the client can refresh it: every
// entity of Kind when ID is empty, else that one.
type Touched struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// Touches collects what one turn's tools changed. Tools of one model round
// run concurrently, so it locks.
type Touches struct {
	mu   sync.Mutex
	list []Touched
}

// Add records a change; a repeat, or one an "every entity of the kind" entry
// already covers, is dropped.
func (t *Touches) Add(kind, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if slices.Contains(t.list, Touched{Kind: kind}) || slices.Contains(t.list, Touched{Kind: kind, ID: id}) {
		return
	}
	if id == "" {
		t.list = slices.DeleteFunc(t.list, func(x Touched) bool { return x.Kind == kind })
	}
	t.list = append(t.list, Touched{Kind: kind, ID: id})
}

// List returns what was changed, in the order first recorded; never nil.
func (t *Touches) List() []Touched {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Touched{}, t.list...)
}

type touchesKey struct{}

// WithTouches returns ctx carrying t for the turn's tools.
func WithTouches(ctx context.Context, t *Touches) context.Context {
	return context.WithValue(ctx, touchesKey{}, t)
}

// Touch records that the running tool changed an entity of kind (id empty
// for many of them). Outside a turn that collects changes it does nothing.
func Touch(ctx context.Context, kind, id string) {
	if t, ok := ctx.Value(touchesKey{}).(*Touches); ok {
		t.Add(kind, id)
	}
}
