package campaign_assistant

import (
	"context"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// groundingOf labels generated posts by the assets that informed them.
func groundingOf(used []AssetRef) assistantkit.Grounding {
	sources := make([]assistantkit.Source, len(used))
	for i, a := range used {
		sources[i] = assistantkit.Source(a)
	}
	return assistantkit.GroundingFrom(sources)
}

// needSources stops generation that would silently come from general
// knowledge: when the campaign is known to have no ready sources and the user
// did not ask for exploratory content, the call becomes a choice to add a
// source first or go ahead, labelled as not grounded. exploratory rebuilds
// the call with that flag set.
func needSources(ctx context.Context, tool string, explicit bool, exploratory func() any) error {
	src := getRequestState(ctx).snapshot.Sources
	if explicit || src == nil || src.Ready > 0 {
		return nil
	}
	return assistantkit.Conflicted(
		"This campaign has no sources yet, so anything I write would come from general knowledge, not your material. "+
			"Add a document or a link on the campaign's Assets tab, or go ahead without them.",
		assistantkit.Option{ID: "exploratory", Title: "Go ahead without sources (marked not grounded)", Tool: tool, Args: exploratory()},
	)
}
