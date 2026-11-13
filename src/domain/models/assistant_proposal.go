package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AssistantProposalStatus is where a proposal is in its life.
type AssistantProposalStatus string

const (
	ProposalPending   AssistantProposalStatus = "pending"
	ProposalConfirmed AssistantProposalStatus = "confirmed"
	ProposalDismissed AssistantProposalStatus = "dismissed"
	ProposalExpired   AssistantProposalStatus = "expired"
)

// AssistantScopeKind is the subject an assistant thread is about.
type AssistantScopeKind string

const (
	AssistantScopeWorkspace AssistantScopeKind = "workspace"
	AssistantScopeCampaign  AssistantScopeKind = "campaign"
	AssistantScopePost      AssistantScopeKind = "post"
)

// ProposalOption is one way forward the user can pick when a proposal offers
// a choice. When Tool is set, choosing the option runs that tool with Args
// instead of the proposal's own call.
type ProposalOption struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
	// Why says why the recommended option is the one to pick.
	Why string `json:"why,omitempty"`
	// Input is the value the option asks for before it runs, which fills
	// the Arg argument of its call.
	Input *ProposalOptionInput `json:"input,omitempty"`
	Tool  string               `json:"tool,omitempty"`
	Args  JSONMap              `json:"args,omitempty"`
}

// ProposalOptionInput is the value an option asks for: its kind, the label
// of its field, and the argument of the option's call it fills.
type ProposalOptionInput struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Arg   string `json:"arg"`
}

// AssistantProposal is work the assistant offered but has not done: a
// proposal-tier tool call with its resolved arguments, stored until the user
// confirms or dismisses it, or it expires. Confirming runs Tool with Args
// exactly as stored, so the model never re-plans an accepted offer.
type AssistantProposal struct {
	bun.BaseModel `bun:"table:assistant_proposals,alias:ap" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string             `bun:"id,pk"                       json:"id"`
	ScopeKind AssistantScopeKind `bun:"scope_kind,notnull"          json:"scope_kind"`
	// ScopeID is the campaign or post id; empty for the workspace.
	ScopeID string `bun:"scope_id,notnull"                   json:"scope_id"`
	Tool    string `bun:"tool,notnull"                       json:"tool"`
	// Args are the tool's input, already resolved (dates, ids, counts).
	Args    JSONMap                 `bun:"args,type:jsonb,notnull"    json:"args"`
	Summary string                  `bun:"summary,notnull"            json:"summary"`
	Options []ProposalOption        `bun:"options,type:jsonb,notnull" json:"options"`
	Status  AssistantProposalStatus `bun:"status,notnull"             json:"status"`
	// Replaces is the confirmed offer this one was made afresh for, because
	// what that one was made on changed; RetryOf is the confirmed offer whose
	// run left the items this one tries again. Each is empty otherwise.
	Replaces string `bun:"replaces,notnull" json:"replaces,omitempty"`
	RetryOf  string `bun:"retry_of,notnull" json:"retry_of,omitempty"`
	// Chosen is the option id the user picked; empty for a plain confirm.
	Chosen     string     `bun:"chosen,notnull"                                json:"chosen,omitempty"`
	CreatedBy  string     `bun:"created_by,notnull"                            json:"created_by"`
	ResolvedBy string     `bun:"resolved_by,notnull"                           json:"resolved_by,omitempty"`
	CreatedAt  time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	ExpiresAt  time.Time  `bun:"expires_at,notnull"                            json:"expires_at"`
	ResolvedAt *time.Time `bun:"resolved_at"                                   json:"resolved_at,omitempty"`
}

// AssistantOfferState is an offer as it stands now, for a thread's history,
// so a reload shows each card as it is rather than as its turn left it.
type AssistantOfferState struct {
	ID string `json:"id"`
	// Status is pending, confirmed, dismissed or expired; a pending offer
	// past its expiry reads expired.
	Status     AssistantProposalStatus `json:"status" swaggertype:"string" enums:"pending,confirmed,dismissed,expired"`
	Chosen     string                  `json:"chosen,omitempty"`
	ExpiresAt  time.Time               `json:"expires_at"`
	ResolvedAt *time.Time              `json:"resolved_at,omitempty"`
	// Replaces and RetryOf are the offers this one follows; ReplacedBy is
	// the fresh offer of what still held when this one's yes found things
	// changed, which is then the one to answer.
	Replaces   string `json:"replaces,omitempty"`
	RetryOf    string `json:"retry_of,omitempty"`
	ReplacedBy string `json:"replaced_by,omitempty"`
}

// State is the proposal as it stands at now.
func (p *AssistantProposal) State(now time.Time) AssistantOfferState {
	s := AssistantOfferState{ID: p.ID, Status: p.Status, Chosen: p.Chosen, ExpiresAt: p.ExpiresAt, ResolvedAt: p.ResolvedAt,
		Replaces: p.Replaces, RetryOf: p.RetryOf}
	if p.Status == ProposalPending && !now.Before(p.ExpiresAt) {
		s.Status = ProposalExpired
	}
	return s
}

// Open reports whether the proposal can still be confirmed or dismissed.
func (p *AssistantProposal) Open(now time.Time) bool {
	return p.Status == ProposalPending && now.Before(p.ExpiresAt)
}

// Option returns the option with the given id.
func (p *AssistantProposal) Option(id string) (ProposalOption, bool) {
	for _, o := range p.Options {
		if o.ID == id {
			return o, true
		}
	}
	return ProposalOption{}, false
}
