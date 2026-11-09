package assistant

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// What a pack's tools read from the turn they run in. Outside a turn — an
// offer confirmed from its card — Scope is the offer's thread and Emit does
// nothing.

// NewTurnContext is a turn of a thread for running pack tools outside the
// orchestrator — a test, or a job acting for a thread. It collects touches
// like a turn does; onEvent may be nil.
func NewTurnContext(ctx context.Context, scope viewcontext.Scope, onEvent OnEventFunc) (context.Context, func() []assistantkit.Touched) {
	return NewViewTurnContext(ctx, viewcontext.ViewContext{Version: viewcontext.Version, Screen: "home", Scope: scope}, onEvent)
}

// NewViewTurnContext is NewTurnContext with the user's screen as view.
func NewViewTurnContext(ctx context.Context, view viewcontext.ViewContext, onEvent OnEventFunc) (context.Context, func() []assistantkit.Touched) {
	r := &viewresolve.Resolved{View: &view}
	if f := view.Focus; f != nil {
		r.Focus = &viewresolve.Item{Kind: f.Kind, ID: f.ID}
	}
	st := newTurnState(r, nil)
	st.emit = onEvent
	return withTurnState(ctx, st), st.touches.List
}

// Scope is the thread the running tool works in.
func Scope(ctx context.Context) (viewcontext.Scope, bool) {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return viewcontext.Scope{}, false
	}
	return st.view.View.Scope, true
}

// Window is the analytics window on the user's screen; nil when none.
func Window(ctx context.Context) *viewcontext.Window {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return nil
	}
	return st.view.View.Window
}

// FocusedAsset is the asset the user's screen shows, on an asset page; ""
// on any other screen. It is the screen the message was sent from, not one a
// navigate in the turn moves to.
func FocusedAsset(ctx context.Context) string {
	st, err := turnStateFrom(ctx)
	if err != nil {
		return ""
	}
	if f := st.view.Focus; f != nil && f.Kind == viewcontext.KindAsset {
		return f.ID
	}
	return ""
}

// CampaignID is the thread's campaign; empty outside a campaign thread.
func CampaignID(ctx context.Context) string {
	if s, ok := Scope(ctx); ok && s.Kind == "campaign" {
		return s.ID
	}
	return ""
}

// Emit streams a pack's own event to the client, e.g. a sub-flow's progress.
// The kinds the core streams itself are dropped.
func Emit(ctx context.Context, kind string, data any) {
	st, err := turnStateFrom(ctx)
	if err != nil || st.emit == nil {
		return
	}
	switch SSEEventKind(kind) {
	case SSEEventExplanationDelta, SSEEventToolCall, SSEEventToolResult, SSEEventComplete, SSEEventError,
		SSEEventPackLoaded, SSEEventUIAction:
		return
	}
	st.emit(SSEEventKind(kind), data)
}
