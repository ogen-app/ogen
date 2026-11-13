package campaign_assistant

import (
	"context"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
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

// The options of the choice generation without sources becomes.
const (
	OptionAddDocument = "add_document"
	OptionImportPage  = "import_page"
	OptionExploratory = "exploratory"
)

// SourceOptions are the ways forward from generation that would come from
// general knowledge: add a document, import a page, or run tool with
// exploratory, its call with the exploratory flag set. Picking one of the
// first two waits for the source, and the request is offered again once
// the source is ready.
func SourceOptions(tool string, exploratory any) []assistantkit.Option {
	return []assistantkit.Option{
		{ID: OptionAddDocument, Title: "Add a document", Description: "Opens the upload on the campaign's Assets tab.", Recommended: true,
			Why: "Posts drawn from your own material say what's true of you, not what's true in general.", Tool: "openAddSource", Args: OpenAddSourceInput{Via: assistant.AddSourceUpload}},
		{ID: OptionImportPage, Title: "Import a page", Description: "Reads a web page and adds it to the campaign's sources.",
			Input: &assistantkit.OptionInput{Kind: assistantkit.OptionInputURL, Label: "Page link", Arg: "url"}, Tool: "importPage", Args: struct{}{}},
		{ID: OptionExploratory, Title: "Go ahead without sources (marked not grounded)", Tool: tool, Args: exploratory},
	}
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
		noSourcesReason+" Offer to add a document or import a page first, or to go ahead without them. If the user replies with a web page's link, pick import_page with it as input. Once the source is read, the request is offered again, grounded in it.",
		SourceOptions(tool, exploratory())...,
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
