package assistantkit

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
)

// Registry holds tool specs in registration order. The order is part of the
// contract: Anthropic caches the compiled strict-tool grammar per exact tool
// list, so the same packs must always yield the same list.
type Registry struct {
	specs  []Spec
	byName map[string]int
}

// NewRegistry validates specs and returns a registry that keeps their order.
func NewRegistry(specs ...Spec) (*Registry, error) {
	r := &Registry{byName: make(map[string]int, len(specs))}
	var errs []error
	for _, s := range specs {
		if err := s.validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := r.byName[s.Name]; dup {
			errs = append(errs, fmt.Errorf("tool %q is registered twice", s.Name))
			continue
		}
		r.byName[s.Name] = len(r.specs)
		r.specs = append(r.specs, s)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return r, nil
}

// Lookup returns the spec registered under name.
func (r *Registry) Lookup(name string) (Spec, bool) {
	i, ok := r.byName[name]
	if !ok {
		return Spec{}, false
	}
	return r.specs[i], true
}

// Specs returns the specs in the given packs, in registration order. No packs
// means every spec.
func (r *Registry) Specs(packs ...string) []Spec {
	out := make([]Spec, 0, len(r.specs))
	for _, s := range r.specs {
		if len(packs) == 0 || slices.Contains(packs, s.Pack) {
			out = append(out, s)
		}
	}
	return out
}

// Tools returns the genkit tools in the given packs, in registration order.
func (r *Registry) Tools(packs ...string) []ai.ToolRef {
	specs := r.Specs(packs...)
	out := make([]ai.ToolRef, len(specs))
	for i, s := range specs {
		out[i] = s.Tool
	}
	return out
}

// Guide renders the "## Tools" section of the prompt for the given packs: one
// "- name: guide" line per tool. A guide's line breaks are folded into spaces,
// so a line of it can't read as another tool.
func (r *Registry) Guide(packs ...string) string {
	var b strings.Builder
	for _, s := range r.Specs(packs...) {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, strings.Join(strings.Fields(s.Guide), " "))
	}
	return b.String()
}

// Actions returns the distinct response actions of the given packs in
// registration order.
func (r *Registry) Actions(packs ...string) []Action {
	var out []Action
	seen := make(map[string]bool)
	for _, s := range r.Specs(packs...) {
		if s.Action == nil || seen[s.Action.Name] {
			continue
		}
		seen[s.Action.Name] = true
		out = append(out, *s.Action)
	}
	return out
}
