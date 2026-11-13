package campaign_assistant

import (
	"context"

	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// groundingOf labels generated ideas by the offered sources they cite. A
// source the model was given but no idea drew on does not count, and an
// exploratory run is exploratory whatever it returned.
func groundingOf(exploratory bool, offered []AssetRef, ideas []content_plan.DraftPost) assistantkit.Grounding {
	if exploratory {
		return assistantkit.GroundingFrom(nil).Because(assistantkit.ChoseWithout)
	}
	cited := make(map[string]bool)
	for _, p := range ideas {
		for _, id := range p.AssetRefs {
			cited[id] = true
		}
	}
	var sources []assistantkit.Source
	for _, a := range offered {
		if cited[a.ID] {
			sources = append(sources, assistantkit.Source{ID: a.ID, Title: a.Title})
		}
	}
	return assistantkit.GroundingFrom(sources).Because(assistantkit.UngroundedBy(len(offered)))
}

// traced names the campaign's analysis of each source, when it has a
// finished one.
func traced(ctx context.Context, st *requestState, g assistantkit.Grounding) assistantkit.Grounding {
	if st.repos.Analyses == nil {
		return g
	}
	return g.WithAnalyses(func(ids []string) (map[string]string, error) {
		return repository.ReadyAnalysisIDs(ctx, st.repos.Analyses, ids, &st.campaignID)
	})
}

const noSourcesReason = "This campaign has no sources yet, so anything I write would come from general knowledge, not your material."

// lacksSources reports whether generation would silently come from general
// knowledge: the campaign is known to have no ready sources and the user did
// not ask for exploratory content.
func lacksSources(ctx context.Context, explicit bool) bool {
	src := getRequestState(ctx).snapshot.Sources
	return !explicit && src != nil && src.Ready == 0
}

// ungroundedReason is why output that drew on none of the campaign's sources
// did: it has none ready, else none was about it.
func ungroundedReason(ctx context.Context) assistantkit.UngroundedReason {
	if src := getRequestState(ctx).snapshot.Sources; src != nil && src.Ready == 0 {
		return assistantkit.NoSources
	}
	return assistantkit.NoneRelevant
}

// needSources stops generation that would silently come from general
// knowledge: the call becomes a choice to add a source first or go ahead,
// labelled as not grounded. exploratory rebuilds the call with that flag set.
func needSources(ctx context.Context, tool string, explicit bool, exploratory func() any) error {
	if !lacksSources(ctx, explicit) {
		return nil
	}
	return assistantkit.Conflicted(
		noSourcesReason+" Offer to add a document or a link first, or to go ahead without them. If the user replies with a web page's link, import it with importPage (it is attached to this campaign) and, once it is read, run the request again.",
		assistantkit.Option{ID: "add_source", Title: "Add a document or a link", Description: "Opens the campaign's Assets tab to upload a document or import a page.", Recommended: true,
			Why: "Posts drawn from your own material say what's true of you, not what's true in general.", Tool: "openAddSource", Args: struct{}{}},
		assistantkit.Option{ID: "exploratory", Title: "Go ahead without sources (marked not grounded)", Tool: tool, Args: exploratory()},
	)
}

// explore returns ctx for a generation run: an exploratory call must not draw
// on the campaign's assets, while the campaign's own setting stays as it is.
func explore(ctx context.Context, exploratory bool) context.Context {
	if exploratory {
		return content_plan.WithoutAssets(ctx)
	}
	return ctx
}
