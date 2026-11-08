package campaign_assistant

import (
	"context"
	"errors"

	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// groundingOf labels generated posts by the offered sources they cite. A
// source the model was given but no post drew on does not count, and an
// exploratory run is exploratory whatever it returned.
func groundingOf(exploratory bool, offered []AssetRef, posts []content_plan.DraftPost) assistantkit.Grounding {
	if exploratory {
		return assistantkit.GroundingFrom(nil)
	}
	cited := make(map[string]bool)
	for _, p := range posts {
		for _, id := range p.AssetRefs {
			cited[id] = true
		}
	}
	var sources []assistantkit.Source
	for _, a := range offered {
		if cited[a.ID] {
			sources = append(sources, assistantkit.Source(a))
		}
	}
	return assistantkit.GroundingFrom(sources)
}

const noSourcesReason = "This campaign has no sources yet, so anything I write would come from general knowledge, not your material."

// lacksSources reports whether generation would silently come from general
// knowledge: the campaign is known to have no ready sources and the user did
// not ask for exploratory content.
func lacksSources(ctx context.Context, explicit bool) bool {
	src := getRequestState(ctx).snapshot.Sources
	return !explicit && src != nil && src.Ready == 0
}

// needSources stops generation that would silently come from general
// knowledge: the call becomes a choice to add a source first or go ahead,
// labelled as not grounded. exploratory rebuilds the call with that flag set.
func needSources(ctx context.Context, tool string, explicit bool, exploratory func() any) error {
	if !lacksSources(ctx, explicit) {
		return nil
	}
	return assistantkit.Conflicted(
		noSourcesReason+" Add a document or a link on the campaign's Assets tab, or go ahead without them.",
		assistantkit.Option{ID: "exploratory", Title: "Go ahead without sources (marked not grounded)", Tool: tool, Args: exploratory()},
	)
}

// withoutSources marks another conflict's options as going ahead without
// sources, so one answer settles both. The options' calls must already carry
// the exploratory flag.
func withoutSources(err error) error {
	c, ok := errors.AsType[*assistantkit.ConflictError](err)
	if !ok {
		return err
	}
	c.Reason = noSourcesReason + " " + c.Reason
	for i := range c.Options {
		c.Options[i].Title += ", without sources (marked not grounded)"
	}
	return c
}

// explore returns ctx for a generation run: an exploratory call must not draw
// on the campaign's assets, while the campaign's own setting stays as it is.
func explore(ctx context.Context, exploratory bool) context.Context {
	if exploratory {
		return content_plan.WithoutAssets(ctx)
	}
	return ctx
}
