// Package assistant is the universal assistant: one thread per workspace,
// campaign or post, that knows the screen each turn was sent from. Its
// orchestrator answers from the resolved view context through the core tools;
// the packs that act on ideas, assets, campaigns and the rest load on top.
package assistant

import (
	"context"

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
	// Unsaved is set when the turn ran but could not be stored in the
	// thread, so it won't be in the history on reload.
	Unsaved bool `json:"unsaved,omitzero"`
}

const (
	actionAnswered = "answered"
	actionDeclined = "declined"
)

// SSEEventKind names an event the turn streams.
type SSEEventKind string

const (
	SSEEventExplanationDelta SSEEventKind = "explanation_delta"
	SSEEventToolCall         SSEEventKind = "tool_call"
	SSEEventToolResult       SSEEventKind = "tool_result"
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
	// OK is false when the tool refused the call or hit a conflict.
	OK bool `json:"ok"`
}

// FlowConfig is the flow's static configuration.
type FlowConfig struct {
	Provider *llm.Provider
	Recorder *usage.Recorder
	Checker  *usage.Checker
	// MaxOutputTokens caps the orchestrator's reply; 0 means 4096.
	MaxOutputTokens int64
	// MaxTurns bounds tool round trips per turn; 0 means 4.
	MaxTurns int
}

// Repos are the stores the flow reads and writes.
type Repos struct {
	Messages repository.AssistantMessageRepository
}

// ValidationError is a turn the server won't run (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is a turn the model failed (HTTP 502).
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// Runner runs one turn.
type Runner func(ctx context.Context, req Request, onEvent OnEventFunc) (*Response, error)
