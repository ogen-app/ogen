// Package assistantkit holds what every assistant tool shares: the descriptor
// a tool registers with, the typed result it returns, and the registry that
// orders tools and renders their part of the system prompt. The prompt's tool
// guide and action rules are generated from the same descriptors the model is
// given, so the two cannot drift apart.
package assistantkit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/firebase/genkit/go/ai"
)

// Tier decides how much a tool may do without asking. The runner enforces it,
// not the prompt.
type Tier string

const (
	// TierRead reads, navigates or filters. Runs straight away.
	TierRead Tier = "read"
	// TierReversible makes a single change that can be undone. Runs straight
	// away; the result carries an undo handle.
	TierReversible Tier = "reversible"
	// TierProposal is bulk, generative, scheduling, external or destructive
	// work. The call is stored as a proposal and runs only once confirmed.
	TierProposal Tier = "proposal"
)

// valid reports whether t is one of the declared tiers.
func (t Tier) valid() bool {
	switch t {
	case TierRead, TierReversible, TierProposal:
		return true
	}
	return false
}

// Scope is the subject an assistant thread is about.
type Scope string

const (
	ScopeWorkspace Scope = "workspace"
	ScopeCampaign  Scope = "campaign"
	ScopePost      Scope = "post"
)

// Action is a response action a tool's success maps to, with the rule the
// model follows when choosing it.
type Action struct {
	Name string
	Rule string
}

// Spec describes one assistant tool.
type Spec struct {
	// Name must equal the genkit tool's name.
	Name string
	// Pack groups tools that load together (e.g. "campaign", "ideas").
	Pack string
	// Scopes the tool is offered in.
	Scopes []Scope
	Tier   Tier
	// Guide is the tool's entry in the prompt: when to call it and how to
	// fill its arguments.
	Guide string
	// Action is the response action a successful call implies. Read tools
	// usually have none.
	Action *Action
	// Tool is the genkit tool the model calls.
	Tool ai.ToolRef
	// Prepare claims what a confirmed run of the tool needs before its offer
	// is marked answered — e.g. the turn's one heavy-action slot — and
	// returns ctx carrying the claim, and a release called once the
	// confirmation is over, which keeps whatever the run went on to use. A
	// Refusal means the run can't start now, so the offer stays open to be
	// confirmed later. nil needs nothing.
	Prepare func(ctx context.Context) (context.Context, func(), error)
	// Free marks a proposal-tier tool whose confirmed run makes no model
	// call, so an offer of it can be confirmed past the tenant's spend cap.
	Free bool
}

// toolName is the tool's name, or false when there is no tool: nil, or a
// typed nil — a nil *ai.ToolDef passes the interface nil check but panics
// in Name. An implementation that answers on a nil receiver still counts.
func toolName(t ai.ToolRef) (name string, ok bool) {
	if t == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			name, ok = "", false
		}
	}()
	return t.Name(), true
}

// validate reports every missing or inconsistent field of the spec at once,
// prefixed with the tool's name.
func (s Spec) validate() error {
	var errs []error
	if s.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if s.Pack == "" {
		errs = append(errs, errors.New("pack is required"))
	}
	if len(s.Scopes) == 0 {
		errs = append(errs, errors.New("at least one scope is required"))
	}
	if !s.Tier.valid() {
		errs = append(errs, fmt.Errorf("unknown tier %q", s.Tier))
	}
	if strings.TrimSpace(s.Guide) == "" {
		errs = append(errs, errors.New("guide is required"))
	}
	if name, ok := toolName(s.Tool); !ok {
		errs = append(errs, errors.New("tool is required"))
	} else if name != s.Name {
		errs = append(errs, fmt.Errorf("tool is named %q", name))
	}
	if s.Action != nil && (s.Action.Name == "" || s.Action.Rule == "") {
		errs = append(errs, errors.New("action needs a name and a rule"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("tool %q: %w", s.Name, err)
	}
	return nil
}
