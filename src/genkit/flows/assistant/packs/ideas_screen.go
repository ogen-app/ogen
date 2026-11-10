package packs

import (
	"context"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// onScreen fills a call's missing ideas from the screen before next runs. It
// goes outside a gate, so an offer stores the ideas meant when it was made
// and confirming it later, from another screen, works on those.
func onScreen[In, Out any](fill func(context.Context, *In), next ai.ToolFunc[In, assistantkit.Outcome[Out]]) ai.ToolFunc[In, assistantkit.Outcome[Out]] {
	return func(ctx *ai.ToolContext, in In) (assistantkit.Outcome[Out], error) {
		fill(ctx, &in)
		return next(ctx, in)
	}
}

// ideasOnScreen is ids, or when none are passed the ideas selected on
// screen, else the one open.
func ideasOnScreen(ctx context.Context, ids []string) []string {
	if len(dedupe(ids)) > 0 {
		return ids
	}
	if sel := assistant.Selected(ctx, viewcontext.KindIdea); len(sel) > 0 {
		return sel
	}
	if id := assistant.Focused(ctx, viewcontext.KindIdea); id != "" {
		return []string{id}
	}
	return ids
}

// ideaOnScreen is id, or when none is passed the idea open on screen, else
// the only one selected.
func ideaOnScreen(ctx context.Context, id string) string {
	if id != "" {
		return id
	}
	if id := assistant.Focused(ctx, viewcontext.KindIdea); id != "" {
		return id
	}
	if sel := assistant.Selected(ctx, viewcontext.KindIdea); len(sel) == 1 {
		return sel[0]
	}
	return ""
}
