package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
	"github.com/ogen-app/ogen/src/usecase/assets"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

const assetsPackName = "assets"

// Asking the assets keeps the best few excerpts above a similarity floor.
const (
	askMinSimilarity = 0.5
	askChunkLimit    = 8
	maxAssetIDs      = 50
)

// assetsPack finds and searches assets, analyses them, tags them, attaches
// them to campaigns and imports pages.
func assetsPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:         assetsPackName,
		Summary:      "find assets and ask what they say, analyse them or distil more ideas, tag them, attach them to a campaign, and import a web page",
		Screens:      []viewcontext.Screen{"assets", "campaign.assets", "asset", "campaign.asset"},
		Instructions: assetsRules,
		Begin:        assetsBegin(d),
		End:          assetsEnd,
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			find := genkit.DefineTool(g, "findAssets",
				"Lists assets matching a filter in the Assets list's syntax, with their id, title, kind and status.",
				assistantkit.Wrap(func(ctx context.Context, in FindAssetsInput) (*FindAssetsOutput, error) {
					return findAssets(ctx, d, in)
				}), ai.WithStrictSchema(false))
			ask := genkit.DefineTool(g, "askAssets",
				"Searches what assets say: returns the passages most relevant to a question, with their asset and pages.",
				assistantkit.Wrap(func(ctx context.Context, in AskAssetsInput) (*AskAssetsOutput, error) {
					out, err := askAssets(ctx, d, in)
					if err == nil {
						recordAssets(ctx, func(r *AssetsResult) { r.Ask = askCard(out) })
					}
					return out, err
				}), ai.WithStrictSchema(false))
			analyse := genkit.DefineTool(g, "analyseAsset",
				"Starts, or re-runs, an asset's analysis — its summary and the ideas distilled from it — for the workspace or for a campaign's lens.",
				assistantkit.Wrap(func(ctx context.Context, in AnalyseAssetInput) (*QueuedOutput, error) {
					return analyseAsset(ctx, d, in)
				}), ai.WithStrictSchema(false))
			distill := genkit.DefineTool(g, "distillMoreIdeas",
				"Asks an analysed asset for more ideas, optionally steered by a focus, without repeating the ones it has.",
				assistantkit.Wrap(func(ctx context.Context, in DistillMoreInput) (*DistillMoreOutput, error) {
					return distillMore(ctx, d, in)
				}), ai.WithStrictSchema(false))
			tag := genkit.DefineTool(g, "tagAssets",
				"Adds or removes tags on assets, by tag name. Above a few assets it is offered for the user to confirm first.",
				assistantkit.Gated("tagAssets", assistantkit.Gate[TagAssetsInput]{
					Precheck: func(_ context.Context, in TagAssetsInput) error { return checkAssetIDs(in.AssetIDs) },
					Recheck: recheckIDs(d, viewcontext.KindAsset, func(_ context.Context, in TagAssetsInput) (string, []string) { return "", in.AssetIDs },
						func(in TagAssetsInput, ids []string) TagAssetsInput { in.AssetIDs = ids; return in }),
					Needs: func(_ context.Context, in TagAssetsInput) bool { return overN(d, len(in.AssetIDs)) },
					Summary: func(in TagAssetsInput) string {
						return fmt.Sprintf("Tag %s", plural(len(in.AssetIDs), "asset", "assets"))
					},
					Card: func(ctx context.Context, in TagAssetsInput) *assistantkit.Card { return tagCard(ctx, d, in) },
				}, assistantkit.Wrap(func(ctx context.Context, in TagAssetsInput) (*TagAssetsOutput, error) { return tagAssets(ctx, d, in) })), ai.WithStrictSchema(false))
			attach := genkit.DefineTool(g, "attachAssets",
				"Attaches assets to a campaign as its sources, or detaches them. Returns an undo id.",
				assistantkit.Wrap(func(ctx context.Context, in AttachAssetsInput) (*AttachAssetsOutput, error) {
					return attachAssets(ctx, d, in)
				}), ai.WithStrictSchema(false))
			importURL := genkit.DefineTool(g, "importPage",
				"Imports a web page as an asset (its text and images), and attaches it to the thread's campaign when there is one.",
				assistantkit.Wrap(func(ctx context.Context, in ImportPageInput) (*ImportPageOutput, error) {
					return importPage(ctx, d, in)
				}), ai.WithStrictSchema(false))

			specs := []assistantkit.Spec{
				spec(assetsPackName, find, assistantkit.TierRead,
					`Call this to look up assets — "which PDFs are still processing?", "assets about pricing". filter is a URL query string in the Assets list's syntax (e.g. text=pricing&status=ready&kind=pdf). To show the user, also call setFilter with it.`),
				spec(assetsPackName, ask, assistantkit.TierRead,
					`Call this when the user asks what their assets say — "what does the deck say about enterprise pricing?". Pass assetIds to search only those (e.g. the selected ones, or the ones the user names). Otherwise, on an asset page it searches the open asset alone, and elsewhere the thread's campaign's assets, or every asset outside a campaign; set allAssets when the user asks beyond the open asset. Answer only from the passages and name the assets you used; when scope is "open_asset", say the answer is from that asset only. If none match, say so.`),
				spec(assetsPackName, analyse, assistantkit.TierRead,
					`Call this when the user asks to analyse an asset, or to re-run a failed analysis. It runs in the background; say the results arrive on the asset page.`),
				spec(assetsPackName, distill, assistantkit.TierRead,
					`Call this when the user wants more ideas from an analysed asset — "give me five more angles on this deck, about pricing". It runs in the background; the ideas appear in the Ideas list.`),
				spec(assetsPackName, tag, assistantkit.TierProposal,
					`Call this to tag or untag assets by tag name; new tag names are created. More than a few assets are offered first. It can be undone.`),
				spec(assetsPackName, attach, assistantkit.TierReversible,
					`Call this when the user asks to use assets in a campaign ("add these to the Spring campaign") or to stop using them (detach). campaignId defaults to the thread's. It can be undone.`),
				spec(assetsPackName, importURL, assistantkit.TierRead,
					`Call this when the user gives a web page to use — "add https://… as a source". The page is read in the background.`),
			}
			if d.Analyses != nil {
				state := genkit.DefineTool(g, "readAssetsState",
					"Reads the asset bank's state for its card: how many assets there are, how their analyses stand, the first waiting and failed ones, and the undecided ideas per lens.",
					assistantkit.Wrap(func(ctx context.Context, _ ReadAssetsStateInput) (*ReadAssetsStateOutput, error) {
						out, err := readAssetsState(ctx, d)
						if err == nil {
							recordAssets(ctx, func(r *AssetsResult) { r.State = out.State })
						}
						return out, err
					}), ai.WithStrictSchema(false))
				specs = append(specs, spec(assetsPackName, state, assistantkit.TierRead,
					`Call this once when the user asks how the bank stands — "where are my assets at?", "what needs doing?" — so the card shows the counts with the assets to analyse or retry. Answer from it; it is the <assets> block, read again.`))
			}
			if d.Analyses != nil && d.IdeaRepo != nil {
				read := genkit.DefineTool(g, "readAnalysis",
					"Reads what an asset's analysis found for one lens — the workspace's or a campaign's: its summary, key points and the ideas it distilled with their verdicts, plus the asset's other lenses.",
					assistantkit.Wrap(func(ctx context.Context, in ReadAnalysisInput) (*ReadAnalysisOutput, error) {
						out, err := readAnalysis(ctx, d, in)
						if err == nil {
							recordAssets(ctx, func(r *AssetsResult) { r.Analysis = out })
						}
						return out, err
					}), ai.WithStrictSchema(false))
				specs = append(specs, spec(assetsPackName, read, assistantkit.TierRead,
					`Call this when the user asks what an analysis found — "what did the analysis find?", "what does it say for Spring reopening?". assetId defaults to the asset open on screen. A lens the user names picks it: pass that campaign's id, or workspace for the workspace lens; none named means the thread's. If status is not_read, pending, failed or skipped, say so plainly and offer what otherLenses has or analyseAsset; never answer from another lens unasked.`))
			}
			if d.Compare != nil {
				compare := genkit.DefineTool(g, "compareAssets",
					"Summarises assets together, or compares them: what they all say, what only some say and where they disagree, each claim tied to its assets. Reads each asset's analysis for the thread's lens, else its passages.",
					assistantkit.Wrap(func(ctx context.Context, in CompareAssetsInput) (*CompareAssetsOutput, error) {
						out, err := compareAssets(ctx, d, in)
						if err == nil {
							recordAssets(ctx, func(r *AssetsResult) { r.Compare = out })
						}
						return out, err
					}), ai.WithStrictSchema(false))
				specs = append(specs, spec(assetsPackName, compare, assistantkit.TierRead,
					`Call this when the user asks to summarise or compare assets — "compare these", "summarise them", "summarise it" — not for one question about them (askAssets). Pass the selected ids from getSelection, or the ones named; it defaults to the asset open on screen. Answer from its result: name the assets by their letter and title, give the disagreements their own line and offer to settle one, and say which assets were read from passages rather than an analysis.`))
			}
			if d.CreateCampaign != nil && d.CampaignTypes != nil {
				setUp := genkit.DefineTool(g, "createCampaignFromAssets",
					"Sets up a new campaign with assets as its sources: its name, type, dates and brief line, the assets attached. Offered for the user to confirm first; returns an undo id.",
					resolveFirst(func(ctx context.Context, in *CampaignFromAssetsInput) { in.AssetIDs = in.sources(ctx) },
						assistantkit.Gated("createCampaignFromAssets", assistantkit.Gate[CampaignFromAssetsInput]{
							Precheck: func(ctx context.Context, in CampaignFromAssetsInput) error {
								_, err := planCampaign(ctx, d, in)
								return err
							},
							Describe: func(ctx context.Context, in CampaignFromAssetsInput) string { return campaignSummary(ctx, d, in) },
							Card: func(ctx context.Context, in CampaignFromAssetsInput) *assistantkit.Card {
								return campaignCard(ctx, d, in)
							},
						}, assistantkit.Wrap(func(ctx context.Context, in CampaignFromAssetsInput) (*CampaignFromAssetsOutput, error) {
							out, err := campaignFromAssets(ctx, d, in)
							if err == nil {
								recordAssets(ctx, func(r *AssetsResult) { r.Campaign = out })
							}
							return out, err
						}))), ai.WithStrictSchema(false))
				setUpSpec := spec(assetsPackName, setUp, assistantkit.TierProposal,
					`Call this when the user asks to set up, start or make a campaign from assets — "set up a campaign from these". Pass the selected ids from getSelection, or the ones named; it defaults to the asset open on screen. Name it, and take the type and dates from what the sources say (an opening date, a launch), with dateReason saying where they come from, leaving dates out when they don't say; the card shows them so the user can change them. It creates no posts: offer a content plan once it's set up.`)
				// Creating the campaign calls no model, so a yes isn't held to the
				// spend cap; its sources' analyses keep their own cap.
				setUpSpec.Free = true
				specs = append(specs, setUpSpec)
			}
			return specs, nil
		},
	}
}

func checkAssetIDs(ids []string) error {
	n := len(dedupe(ids))
	if n == 0 {
		return assistantkit.Refused("pass the assets' ids; get them from getSelection or findAssets")
	}
	if n > maxAssetIDs {
		return assistantkit.Refused(fmt.Sprintf("at most %d assets at once", maxAssetIDs))
	}
	return nil
}

// FindAssetsInput is an Assets list query.
type FindAssetsInput struct {
	Filter string `json:"filter,omitempty" jsonschema:"description=A URL query string in the Assets list's syntax"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"description=At most 50"`
}

// AssetRef is an asset as the assistant sees it.
type AssetRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// FindAssetsOutput is one page of matches.
type FindAssetsOutput struct {
	Assets []AssetRef `json:"assets"`
	Total  int        `json:"total"`
}

func findAssets(ctx context.Context, d Deps, in FindAssetsInput) (*FindAssetsOutput, error) {
	v, err := url.ParseQuery(in.Filter)
	if err != nil {
		return nil, assistantkit.Refused("the filter is not a query string")
	}
	q, err := assets.ParseQuery(v, time.Now())
	if err != nil {
		return nil, assistantkit.Refused("the Assets list rejects that filter: " + err.Error())
	}
	q.ScopeCampaign = assistant.CampaignID(ctx)
	q.Limit = 20
	if in.Limit > 0 {
		q.Limit = min(in.Limit, 50)
	}
	res, err := d.Assets.Query(ctx, q)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assistantkit.Refused("this thread's campaign is gone")
	}
	if err != nil {
		return nil, err
	}
	out := &FindAssetsOutput{Assets: make([]AssetRef, len(res.Assets)), Total: res.Total}
	for i, a := range res.Assets {
		out.Assets[i] = AssetRef{ID: a.ID, Title: a.Title, Kind: string(a.Kind), Status: a.Status}
	}
	return out, nil
}

// AskAssetsInput is a question for the assets.
type AskAssetsInput struct {
	Question string   `json:"question"`
	AssetIDs []string `json:"assetIds,omitempty" jsonschema:"description=Search only these assets"`
	// AllAssets widens a question asked on an asset page past that asset.
	AllAssets bool `json:"allAssets,omitempty" jsonschema:"description=Search the campaign's or the workspace's assets, not only the asset open on screen"`
}

// What a question searched.
const (
	scopeAsked     = "asked"
	scopeOpenAsset = "open_asset"
	scopeCampaign  = "campaign"
	scopeWorkspace = "workspace"
)

// Excerpt is a passage that answers a question.
type Excerpt struct {
	AssetID string `json:"assetId"`
	Title   string `json:"title"`
	// Type is the asset's kind: PDF, DOC, IMG, AUDIO, URL or MD.
	Type  string `json:"type,omitempty"`
	Pages string `json:"pages,omitempty"`
	Text  string `json:"text"`
}

// AskAssetsOutput is the best passages, or why there are none.
type AskAssetsOutput struct {
	Excerpts []Excerpt `json:"excerpts"`
	// Scope is what was searched: the assets asked for, the asset open on
	// screen, the thread's campaign's assets or the workspace's.
	Scope string `json:"scope"`
	Note  string `json:"note,omitempty"`
	// Step is the asset searched when it was one, and the passages found.
	Step *assistantkit.Step `json:"step,omitempty"`
}

func askAssets(ctx context.Context, d Deps, in AskAssetsInput) (*AskAssetsOutput, error) {
	question := strings.TrimSpace(in.Question)
	if question == "" {
		return nil, assistantkit.Refused("ask a question about the assets")
	}
	if !embedopts.Available(d.Embedder) || d.Chunks == nil {
		return &AskAssetsOutput{Excerpts: []Excerpt{}, Note: "asset search is unavailable right now"}, nil
	}
	ids, scope, err := searchScope(ctx, d, dedupe(in.AssetIDs), in.AllAssets)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		note := "there are no ready assets to search"
		if scope == scopeOpenAsset {
			note = "the open asset isn't ready to search yet; set allAssets to search the others"
		}
		return &AskAssetsOutput{Excerpts: []Excerpt{}, Scope: scope, Note: note}, nil
	}
	emb, err := d.Embedder.Embed(ctx, &ai.EmbedRequest{Input: []*ai.Document{ai.DocumentFromText(question, nil)}, Options: embedopts.Query()})
	if err != nil || len(emb.Embeddings) == 0 {
		return &AskAssetsOutput{Excerpts: []Excerpt{}, Scope: scope, Note: "asset search is unavailable right now"}, nil
	}
	chunks, err := d.Chunks.SearchSimilar(ctx, pgvector.NewHalfVector(emb.Embeddings[0].Embedding), ids, askMinSimilarity, askChunkLimit)
	if err != nil {
		return nil, fmt.Errorf("search assets: %w", err)
	}
	metas := map[string]models.Asset{}
	if as, err := d.Assets.ListMeta(ctx, ids); err == nil {
		for _, a := range as {
			metas[a.ID] = a
		}
	}
	out := &AskAssetsOutput{Excerpts: make([]Excerpt, 0, len(chunks)), Scope: scope}
	for _, c := range chunks {
		m := metas[c.AssetID]
		out.Excerpts = append(out.Excerpts, Excerpt{AssetID: c.AssetID, Title: m.Title, Type: deref(m.Type), Pages: pages(c.PageStart, c.PageEnd), Text: c.Content})
	}
	if len(out.Excerpts) == 0 {
		out.Note = "no asset content matched the question"
	}
	out.Step = &assistantkit.Step{Passages: len(out.Excerpts)}
	if len(ids) == 1 {
		out.Step.Subject = metas[ids[0]].Title
	}
	return out, nil
}

// searchScope is the ready assets a question searches, and which they are:
// the ones asked for, else the asset open on screen unless all is set, else
// the thread's campaign's, else the workspace's.
func searchScope(ctx context.Context, d Deps, asked []string, all bool) ([]string, string, error) {
	var candidates []string
	scope := scopeWorkspace
	switch open := assistant.FocusedAsset(ctx); {
	case len(asked) > 0:
		candidates, scope = asked, scopeAsked
	case open != "" && !all:
		candidates, scope = []string{open}, scopeOpenAsset
	case assistant.CampaignID(ctx) != "":
		scope = scopeCampaign
		c, err := d.Campaigns.GetByID(ctx, assistant.CampaignID(ctx))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, scope, assistantkit.Refused("this thread's campaign is gone")
		}
		if err != nil {
			return nil, scope, err
		}
		candidates = c.AssetIDs
		if len(candidates) == 0 {
			return nil, scope, nil
		}
	}
	// nil lists every asset of the tenant.
	metas, err := d.Assets.ListMeta(ctx, candidates)
	if err != nil {
		return nil, scope, err
	}
	var ready []string
	for _, a := range metas {
		if a.Status == "ready" || a.Status == "partial" {
			ready = append(ready, a.ID)
		}
	}
	return ready, scope, nil
}

func pages(start, end *int) string {
	switch {
	case start == nil:
		return ""
	case end == nil || *end == *start:
		return fmt.Sprintf("p. %d", *start)
	}
	return fmt.Sprintf("pp. %d–%d", *start, *end)
}

// AnalyseAssetInput names the lens to analyse.
type AnalyseAssetInput struct {
	AssetID    string `json:"assetId"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Analyse for this campaign's lens; defaults to the thread's campaign, else the workspace lens"`
}

// QueuedOutput is background work that started.
type QueuedOutput struct {
	Queued bool   `json:"queued"`
	Note   string `json:"note"`
}

func lensCampaign(ctx context.Context, asked string) *string {
	if id := strings.TrimSpace(asked); id != "" {
		return &id
	}
	if id := assistant.CampaignID(ctx); id != "" {
		return &id
	}
	return nil
}

func analyseAsset(ctx context.Context, d Deps, in AnalyseAssetInput) (*QueuedOutput, error) {
	if d.Analysis == nil {
		return nil, assistantkit.Refused("asset analysis is not available")
	}
	tenant, _ := tenantctx.From(ctx)
	err := d.Analysis.Retry(ctx, tenant, in.AssetID, lensCampaign(ctx, in.CampaignID))
	switch {
	case errors.Is(err, assetanalysis.ErrNotRetryable):
		return nil, assistantkit.Refused("that analysis is already done or running; ask for more ideas instead")
	case errors.Is(err, sql.ErrNoRows):
		return nil, assistantkit.Refused("that asset isn't in the campaign, or doesn't exist")
	case err != nil:
		return nil, err
	}
	assistantkit.Touch(ctx, "asset", in.AssetID)
	return &QueuedOutput{Queued: true, Note: "the analysis runs in the background; its summary and ideas appear on the asset"}, nil
}

// DistillMoreInput asks an analysed asset for more ideas.
type DistillMoreInput struct {
	AssetID    string `json:"assetId"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=The campaign lens; defaults to the thread's campaign, else the workspace lens"`
	Focus      string `json:"focus,omitempty"      jsonschema:"description=What the new ideas should be about"`
	Count      int    `json:"count,omitempty"      jsonschema:"description=How many ideas; the service caps it"`
}

// DistillMoreOutput is whether more ideas were asked for, and what they
// are grounded in: the asset, through the analysis they are distilled from.
type DistillMoreOutput struct {
	QueuedOutput
	Grounding assistantkit.Grounding `json:"grounding"`
}

func distillMore(ctx context.Context, d Deps, in DistillMoreInput) (*DistillMoreOutput, error) {
	if d.Analysis == nil {
		return nil, assistantkit.Refused("asset analysis is not available")
	}
	tenant, _ := tenantctx.From(ctx)
	by := actor(ctx)
	run, err := d.Analysis.RequestDistill(ctx, assetanalysis.DistillInput{
		TenantID: tenant, AssetID: in.AssetID, CampaignID: lensCampaign(ctx, in.CampaignID), Focus: in.Focus, Count: in.Count, UserID: &by,
	})
	src := assistantkit.Source{ID: in.AssetID}
	if run != nil {
		src.AnalysisID = run.AnalysisID
	}
	out := &DistillMoreOutput{Grounding: newAssetTitles(d).grounding(ctx, []assistantkit.Source{src})}
	switch {
	case errors.Is(err, assetanalysis.ErrDistillInProgress):
		out.QueuedOutput = QueuedOutput{Queued: false, Note: "more ideas are already being distilled from this asset"}
		recordAssets(ctx, func(r *AssetsResult) { r.Distill = out })
		return out, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, assistantkit.Refused("that asset isn't analysed for this lens yet; analyse it first")
	case err != nil:
		return nil, assistantkit.Refused("couldn't ask for more ideas: " + err.Error())
	}
	assistantkit.Touch(ctx, "idea", "")
	out.QueuedOutput = QueuedOutput{Queued: true, Note: "the new ideas appear in the Ideas list when they're ready"}
	recordAssets(ctx, func(r *AssetsResult) { r.Distill = out })
	return out, nil
}

// TagAssetsInput adds and removes tags by name.
type TagAssetsInput struct {
	AssetIDs []string `json:"assetIds"`
	Add      []string `json:"add,omitempty"    jsonschema:"description=Tag names to add; new names are created"`
	Remove   []string `json:"remove,omitempty" jsonschema:"description=Tag names to remove"`
}

// tagCard is the tagging offer's card: the tags added and removed, on which
// assets.
func tagCard(ctx context.Context, d Deps, in TagAssetsInput) *assistantkit.Card {
	ids := dedupe(in.AssetIDs)
	metas, err := d.Assets.ListMeta(ctx, ids)
	if err != nil || len(metas) == 0 {
		return nil
	}
	verb, action := "Tag ", assistantkit.CardActionTag
	if len(in.Add) == 0 {
		verb, action = "Untag ", assistantkit.CardActionUntag
	}
	c := &assistantkit.Card{
		Verb:    verb + plural(len(ids), "asset", "assets"),
		Action:  action,
		Count:   len(ids),
		Sources: cardSources(metas),
	}
	if len(in.Add) > 0 {
		c.Rows = append(c.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowTags, Value: "Add " + strings.Join(in.Add, ", "), Add: in.Add})
	}
	if len(in.Remove) > 0 {
		c.Rows = append(c.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowTags, Value: "Remove " + strings.Join(in.Remove, ", "), Remove: in.Remove})
	}
	return c
}

// TagAssetsOutput is how many assets were tagged, with which tags, and what
// changed on each.
type TagAssetsOutput struct {
	Tagged int `json:"tagged"`
	// Changed counts the assets whose tags changed; the rest already were
	// as asked, and undo leaves them alone.
	Changed int `json:"changed"`
	// Add and Remove are the tags' names, as the tag list has them.
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// Assets are the first few assets with what changed on each; More
	// counts the rest.
	Assets []TaggedAsset `json:"assets"`
	More   int           `json:"more,omitzero"`
	// UndoID puts the tags back on the assets the call changed, all at once.
	UndoID string `json:"undoId,omitempty"`
}

// TaggedAsset is one asset a tagging was for: the tags it gained and lost,
// by name; neither when it already was as asked.
type TaggedAsset struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// taggedShown is how many assets a tagging's result lists.
const taggedShown = 5

// tagNames names the tag ids in has but not in hadNot, by names.
func tagNames(names map[string]string, has, hadNot []string) []string {
	var out []string
	for _, id := range has {
		if !slices.Contains(hadNot, id) {
			out = append(out, cmp.Or(names[id], id))
		}
	}
	return out
}

func tagAssets(ctx context.Context, d Deps, in TagAssetsInput) (*TagAssetsOutput, error) {
	if d.Tags == nil {
		return nil, assistantkit.Refused("tagging is not available")
	}
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return nil, assistantkit.Refused("name the tags to add or remove")
	}
	var add, remove []string
	names := map[string]string{}
	out := &TagAssetsOutput{Assets: []TaggedAsset{}}
	for _, name := range dedupe(in.Add) {
		t, err := d.Tags.FindOrCreateByName(ctx, name, "", actor(ctx))
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", name, err)
		}
		add, names[t.ID], out.Add = append(add, t.ID), t.Name, append(out.Add, t.Name)
	}
	if len(in.Remove) > 0 {
		all, err := d.Tags.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range all {
			if slices.ContainsFunc(in.Remove, func(n string) bool { return strings.EqualFold(strings.TrimSpace(n), t.Name) }) {
				remove, names[t.ID], out.Remove = append(remove, t.ID), t.Name, append(out.Remove, t.Name)
			}
		}
	}
	updated, was, err := d.Assets.ApplyTagsBefore(ctx, dedupe(in.AssetIDs), add, remove)
	if err != nil {
		return nil, err
	}
	assistantkit.Touch(ctx, "asset", "")
	out.Tagged = len(updated)
	// Only the assets whose tags changed are put back, so one the call left
	// as it was can't block the undo.
	before, after := map[string][]string{}, map[string][]string{}
	for _, a := range updated {
		if !slices.Equal(was[a.ID], a.TagIDs) {
			before[a.ID], after[a.ID] = was[a.ID], a.TagIDs
		}
		if len(out.Assets) == taggedShown {
			out.More++
			continue
		}
		out.Assets = append(out.Assets, TaggedAsset{ID: a.ID, Title: a.Title,
			Added: tagNames(names, a.TagIDs, was[a.ID]), Removed: tagNames(names, was[a.ID], a.TagIDs)})
	}
	out.Changed = len(after)
	if len(after) > 0 {
		out.UndoID = recordUndo(ctx, d, models.AssistantActionAssetTags, "", assistantundo.AssetTags(before), assistantundo.AssetTags(after))
	}
	return out, nil
}

// AttachAssetsInput attaches assets to a campaign, or detaches them.
type AttachAssetsInput struct {
	AssetIDs   []string `json:"assetIds"`
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
	Detach     bool     `json:"detach,omitempty"     jsonschema:"description=Stop using them in the campaign instead"`
}

// AttachAssetsOutput is the campaign's sources now.
type AttachAssetsOutput struct {
	CampaignID string   `json:"campaignId"`
	AssetIDs   []string `json:"assetIds"`
	UndoID     string   `json:"undoId,omitempty"`
}

func attachAssets(ctx context.Context, d Deps, in AttachAssetsInput) (*AttachAssetsOutput, error) {
	if err := checkAssetIDs(in.AssetIDs); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(in.CampaignID)
	if id == "" {
		id = assistant.CampaignID(ctx)
	}
	if id == "" {
		return nil, assistantkit.Refused("which campaign? pass campaignId")
	}
	ids := dedupe(in.AssetIDs)
	var add, remove []string
	if in.Detach {
		remove = ids
	} else {
		// Only the tenant's own assets attach.
		metas, err := d.Assets.ListMeta(ctx, ids)
		if err != nil {
			return nil, err
		}
		if len(metas) == 0 {
			return nil, assistantkit.Refused("none of those assets exist")
		}
		add = make([]string, len(metas))
		for i, a := range metas {
			add[i] = a.ID
		}
	}
	// One write under the campaign's row lock, which also keeps use_assets in
	// step with the set: the batch lands whole or not at all, and the undo
	// snapshots hold no concurrent attach.
	was, c, err := d.Campaigns.ChangeAssetIDs(ctx, id, add, remove)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assistantkit.Refused("that campaign doesn't exist")
	}
	if err != nil {
		return nil, err
	}
	if d.Analysis != nil && !in.Detach {
		tenant, _ := tenantctx.From(ctx)
		d.Analysis.ScheduleAttached(ctx, tenant, c.ID, ids)
	}
	assistantkit.Touch(ctx, "campaign", c.ID)
	undo := recordUndo(ctx, d, models.AssistantActionCampaignAssets, c.ID, assistantundo.CampaignAssets(was), assistantundo.CampaignAssets(c))
	return &AttachAssetsOutput{CampaignID: c.ID, AssetIDs: c.AssetIDs, UndoID: undo}, nil
}

// ImportPageInput is a web page to import.
type ImportPageInput struct {
	URL string `json:"url"`
}

// ImportPageOutput is the asset holding the page.
type ImportPageOutput struct {
	AssetID  string `json:"assetId"`
	Title    string `json:"title"`
	Created  bool   `json:"created"`
	Attached bool   `json:"attached"`
	// AttachError says why the page was imported but not attached to the
	// thread's campaign.
	AttachError string `json:"attachError,omitempty"`
}

func importPage(ctx context.Context, d Deps, in ImportPageInput) (*ImportPageOutput, error) {
	if d.URLImport == nil || !d.URLImport.Configured(ctx) {
		return nil, assistantkit.Refused("importing web pages is not available")
	}
	// The same page with a fragment or other host casing finds the asset
	// already holding it instead of importing a duplicate.
	pageURL, err := urlimport.Normalize(ctx, in.URL)
	if invalid, ok := errors.AsType[*urlimport.InvalidURLError](err); ok {
		return nil, assistantkit.Refused("couldn't import that page: " + invalid.Msg)
	}
	if err != nil {
		return nil, err
	}
	tenant, _ := tenantctx.From(ctx)
	res, err := d.URLImport.Import(ctx, urlimport.Input{TenantID: tenant, UserID: actor(ctx), URL: pageURL})
	if err != nil {
		return nil, assistantkit.Refused("couldn't import that page: " + err.Error())
	}
	out := &ImportPageOutput{AssetID: res.Asset.ID, Title: res.Asset.Title, Created: res.Created}
	assistantkit.Touch(ctx, "asset", res.Asset.ID)
	if assistant.CampaignID(ctx) != "" {
		_, err := attachAssets(ctx, d, AttachAssetsInput{AssetIDs: []string{res.Asset.ID}})
		switch r, refused := errors.AsType[*assistantkit.Refusal](err); {
		case err == nil:
			out.Attached = true
		case refused:
			out.AttachError = "imported, but not attached to the campaign: " + r.Reason
		default:
			slog.WarnContext(ctx, "attach imported page failed", logging.AttrComponent, "assistant.packs", "asset_id", res.Asset.ID, logging.AttrError, err)
			out.AttachError = "imported, but attaching it to the campaign failed; retry with attachAssets and this assetId"
		}
	}
	return out, nil
}
