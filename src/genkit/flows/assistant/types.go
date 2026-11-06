// Package assistant is the universal assistant: one thread per workspace,
// campaign or post, that knows the screen each turn was sent from. Its
// orchestrator answers from the resolved view context through the core tools;
// the packs that act on ideas, assets, campaigns and the rest load on top.
package assistant

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// Request is one turn. View is the turn's view context, already resolved in
// the caller's tenant; its scope names the thread.
type Request struct {
	Instruction string
	View        *viewresolve.Resolved
}

// Response is a turn's result, and what the "complete" event carries.
type Response struct {
	Explanation string `json:"explanation"`
	// Action is "answered", or "declined" for a request out of scope.
	Action string `json:"action" enums:"answered,declined"`
	// UIActions drive the client's screen, in order. Each was also streamed
	// as a "ui_action" event just before "complete".
	UIActions []UIAction `json:"uiActions,omitempty"`
	// Touched is what the turn changed, for the client to refresh; [] when
	// nothing.
	Touched []assistantkit.Touched `json:"touched"`
	// Packs are the packs the turn ran with, besides the core tools.
	Packs []string `json:"packs,omitempty"`
	// Proposals are the offers this turn made, waiting for the user's yes;
	// each was also streamed as "proposal_created".
	Proposals []ProposalCard `json:"proposals,omitempty"`
	// Result is the work's result when an offer was answered from its card.
	Result any `json:"result,omitempty"`
}

const (
	actionAnswered = "answered"
	actionDeclined = "declined"
)

// SSEEventKind names an event the turn streams. In order:
//
//   - explanation_delta {delta}: a fragment of the reply, as it streams.
//   - tool_call {name, input, ref} and tool_result {name, ref, ok}: a tool
//     the orchestrator ran.
//   - pack_loaded {pack}: the turn restarted with another pack's tools;
//     the deltas streamed before it are void.
//   - proposal_created ProposalCard / proposal_resolved ProposalResolved:
//     an offer made, or answered, this turn; once the reply is final.
//   - ui_action UIAction: a change to the client's screen, in order, once
//     the reply is final.
//   - complete Response: the turn's result. Last.
//   - error {message, code}: the turn failed; nothing follows.
type SSEEventKind string

const (
	SSEEventExplanationDelta SSEEventKind = "explanation_delta"
	SSEEventToolCall         SSEEventKind = "tool_call"
	SSEEventToolResult       SSEEventKind = "tool_result"
	SSEEventPackLoaded       SSEEventKind = "pack_loaded"
	SSEEventUIAction         SSEEventKind = "ui_action"
	SSEEventProposalCreated  SSEEventKind = "proposal_created"
	SSEEventProposalResolved SSEEventKind = "proposal_resolved"
	SSEEventComplete         SSEEventKind = "complete"
	SSEEventError            SSEEventKind = "error"
)

// OnEventFunc receives the turn's events; nil streams nothing.
type OnEventFunc func(kind SSEEventKind, data any)

// DeltaEventPayload is an "explanation_delta" fragment.
type DeltaEventPayload struct {
	Delta string `json:"delta"`
}

// ToolCallEventPayload is a "tool_call" event.
type ToolCallEventPayload struct {
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

// ToolResultEventPayload is a "tool_result" event.
type ToolResultEventPayload struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	OK   bool   `json:"ok"`
}

// PackLoadedEventPayload is a "pack_loaded" event.
type PackLoadedEventPayload struct {
	Pack string `json:"pack"`
}

// FlowConfig is the flow's static configuration.
type FlowConfig struct {
	Provider *llm.Provider
	Recorder *usage.Recorder
	Checker  *usage.Checker
	// MaxOutputTokens caps the orchestrator's reply; 0 means 4096.
	MaxOutputTokens int64
	// MaxTurns bounds tool round trips per model call; 0 means 4.
	MaxTurns int
	// PrewarmTools compiles Anthropic's strict-tool grammar for every default
	// pack combination at init, off the user's path.
	PrewarmTools bool
}

// Repos are the stores the flow reads and writes.
type Repos struct {
	Messages repository.AssistantMessageRepository
	// Views reloads the references the UI tools are given.
	Views ViewReader
	// SavedViews finds the saved view openSavedView opens.
	SavedViews SavedViewReader
	// Proposals stores the offers pack tools hold back for a yes; nil
	// leaves the proposal-tier tools unable to run and drops
	// confirmOffer / dismissOffer.
	Proposals repository.AssistantProposalRepository
	// ForeignAnswer answers an offer another assistant made in the same
	// thread — the campaign assistant's, through the campaign pack — so a
	// "yes" here runs it. nil refuses such offers.
	ForeignAnswer func(ctx context.Context, req AnswerRequest) (any, error)
}

// ViewReader reloads references in the caller's tenant;
// viewresolve.Resolver.
type ViewReader interface {
	Items(ctx context.Context, kind viewcontext.EntityKind, ids []string) ([]viewresolve.Item, error)
	CheckFilter(kind viewcontext.EntityKind, filter string) error
}

// SavedViewReader reads one saved view; repository.SavedViewRepository.
type SavedViewReader interface {
	GetByID(ctx context.Context, id string) (*models.SavedView, error)
}

// ValidationError is a turn the server won't run (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is a turn the model failed (HTTP 502).
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// Runner runs one turn.
type Runner func(ctx context.Context, req Request, onEvent OnEventFunc) (*Response, error)

// Assistant is the initialised flow.
type Assistant struct {
	// Run runs one turn.
	Run Runner
	// Answer confirms or dismisses one of this assistant's offers from its
	// card, without a model call. Another assistant's offer is
	// ErrNotThisAssistant.
	Answer func(ctx context.Context, req AnswerRequest, onEvent OnEventFunc) (*Response, error)
}

// ErrNotThisAssistant is an offer another assistant made.
var ErrNotThisAssistant = errNotMine
