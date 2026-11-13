package packs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// What getIdea returns of the analysis an idea came from.
const ideaAnalysisRunes = 600

// maxCommonIdeas caps the ideas one ideasInCommon call compares.
const maxCommonIdeas = 50

// defineReadTools defines getIdea and ideasInCommon.
func defineReadTools(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	get := genkit.DefineTool(g, "getIdea",
		"Returns one idea in full: its note, angle and rationale, the source passages it rests on, why it scored its fit (each criterion's level and reason, and any cap), the asset and analysis it came from, and the posts made from it.",
		onScreen(func(ctx context.Context, in *GetIdeaInput) { in.IdeaID = ideaOnScreen(ctx, in.IdeaID) },
			assistantkit.Wrap(recorded(ideasPackName, "getIdea", nil, func(ctx context.Context, in GetIdeaInput) (*GetIdeaOutput, error) {
				return getIdea(ctx, d, in)
			}))), ai.WithStrictSchema(false))
	common := genkit.DefineTool(g, "ideasInCommon",
		"Works out what several ideas share: the assets they came from, their campaign, origin, the lens that scored them, their planned platform, type and phase, and the words their titles and angles have in common.",
		onScreen(func(ctx context.Context, in *IdeasInCommonInput) { in.IdeaIDs = ideasOnScreen(ctx, in.IdeaIDs) },
			assistantkit.Wrap(recorded(ideasPackName, "ideasInCommon", nil, func(ctx context.Context, in IdeasInCommonInput) (*IdeasInCommonOutput, error) {
				return ideasInCommon(ctx, d, in)
			}))), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(ideasPackName, get, assistantkit.TierRead,
			`Call this when the user asks about one idea — "why does this fit?", "why did it score 80?", "where does this come from?", "has it been used?". ideaId defaults to the idea open on screen, else the one selected. Explain a fit from its criteria's reasons; never invent one. Say where in its source it came from by sourceAsset's label (a page, slide or time) when it has one.`),
		spec(ideasPackName, common, assistantkit.TierRead,
			`Call this when the user asks what several ideas have in common, or what ties them together — "what do the selected ideas share?". ideaIds default to the selected ideas; 2 to 50. Describe the shared themes from what it returns, and say when they share little.`),
	}
}

// GetIdeaInput names the idea to read.
type GetIdeaInput struct {
	IdeaID string `json:"ideaId,omitempty" jsonschema:"description=Defaults to the idea open on screen, else the one selected"`
}

// GetIdeaOutput is one idea in full.
type GetIdeaOutput struct {
	Idea      IdeaRef  `json:"idea"`
	Note      string   `json:"note,omitempty"`
	Angle     string   `json:"angle,omitempty"`
	Rationale string   `json:"rationale,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	// Fit is why the idea scored what it did; nil for an idea no lens scored.
	Fit         *IdeaFit      `json:"fit,omitempty"`
	SourceAsset *AssetName    `json:"sourceAsset,omitempty"`
	Analysis    *IdeaAnalysis `json:"analysis,omitempty"`
	// Posts are the posts made from it, oldest first; empty while unused.
	Posts     []models.IdeaPostRef `json:"posts"`
	Author    string               `json:"author,omitempty"`
	CreatedAt time.Time            `json:"createdAt"`
	Note2     string               `json:"explain,omitempty"`
}

// AssetName is an asset by id and title, with where in it the idea came
// from.
type AssetName struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Passage
}

// Passage is where in an asset an idea came from, to open the asset there.
// Every field is empty when no passage was recorded: the idea predates it, or
// the asset's body has no chunks.
type Passage struct {
	ChunkID string `json:"chunkId,omitempty"`
	// Label is the passage as cited: "p. 12", "Slide 4", "12:03–12:47".
	Label   string `json:"label,omitempty"`
	Page    *int   `json:"page,omitempty"`
	PageEnd *int   `json:"pageEnd,omitempty"`
	Slide   *int   `json:"slide,omitempty"`
	StartMS *int64 `json:"startMs,omitempty"`
	EndMS   *int64 `json:"endMs,omitempty"`
}

// passageOf is loc as a Passage; empty for nil.
func passageOf(loc *models.SourceLocator) Passage {
	if loc == nil {
		return Passage{}
	}
	return Passage{ChunkID: loc.ChunkID, Label: loc.Label, Page: loc.Page, PageEnd: loc.PageEnd, Slide: loc.Slide, StartMS: loc.StartMS, EndMS: loc.EndMS}
}

// IdeaFit is how a lens rated an idea.
type IdeaFit struct {
	Score *int `json:"score,omitempty"`
	// Lens is workspace or campaign; CampaignID the campaign lens's.
	Lens       string         `json:"lens"`
	CampaignID string         `json:"campaignId,omitempty"`
	Criteria   []FitCriterion `json:"criteria"`
	Caps       []FitCap       `json:"caps,omitempty"`
}

// FitCriterion is one criterion's rating, 1 to 5, with its reason.
type FitCriterion struct {
	Key      string `json:"key"`
	Question string `json:"question"`
	Weight   int    `json:"weight"`
	Level    int    `json:"level"`
	Means    string `json:"means"`
	Why      string `json:"why"`
	Phase    string `json:"phase,omitempty"`
	Campaign string `json:"campaign,omitempty"`
}

// FitCap is a flaw that capped the score, and what it means.
type FitCap struct {
	Cap     string `json:"cap"`
	Meaning string `json:"meaning"`
}

// IdeaAnalysis is the analysis an idea came from, in brief.
type IdeaAnalysis struct {
	ID   string  `json:"id"`
	Lens LensRef `json:"lens"`
	Gist string  `json:"gist,omitempty"`
	// Summary is cut to a few hundred characters; readAnalysis has it all.
	Summary string `json:"summary,omitempty"`
}

func getIdea(ctx context.Context, d Deps, in GetIdeaInput) (*GetIdeaOutput, error) {
	if in.IdeaID == "" {
		return nil, assistantkit.Refused("which idea? open it, select it, or pass ideaId")
	}
	idea, err := d.Ideas.Get(ctx, in.IdeaID)
	if err != nil {
		return nil, triageErr(err)
	}
	out := &GetIdeaOutput{Idea: ideaRef(*idea), Note: idea.Note, Angle: deref(idea.Angle), Rationale: deref(idea.Rationale),
		Sources: idea.Sources, Fit: ideaFit(idea), Posts: []models.IdeaPostRef{}, CreatedAt: idea.CreatedAt}
	if idea.Origin == models.IdeaOriginManual {
		out.Author = idea.CreatedByName
	}
	if out.Fit == nil {
		out.Note2 = "no lens scored this idea: it was written by hand, or analysed before fit scores existed"
	}
	if idea.SourceAssetID != nil && d.Assets != nil {
		if metas, err := d.Assets.ListMeta(ctx, []string{*idea.SourceAssetID}); err == nil && len(metas) == 1 {
			out.SourceAsset = &AssetName{ID: metas[0].ID, Title: metas[0].Title, Passage: passageOf(idea.SourceLocator)}
		}
	}
	if idea.SourceAssetID != nil && idea.AssetAnalysisID != nil && d.Analyses != nil {
		out.Analysis = ideaAnalysis(ctx, d, *idea.SourceAssetID, *idea.AssetAnalysisID)
	}
	if d.Links != nil {
		posts, err := d.Links.PostsOfIdea(ctx, idea.ID)
		if err != nil {
			return nil, fmt.Errorf("read posts made from the idea: %w", err)
		}
		out.Posts = posts
	}
	return out, nil
}

// ideaFit is an idea's ratings with the rubric's questions and level
// meanings, in rubric order; nil when no lens scored it.
func ideaFit(idea *models.Idea) *IdeaFit {
	b := idea.FitBreakdown
	if b == nil {
		return nil
	}
	f := &IdeaFit{Score: idea.FitScore, Lens: string(b.Lens), Criteria: []FitCriterion{}}
	if b.CampaignID != nil {
		f.CampaignID = *b.CampaignID
	}
	for _, c := range ideafit.Criteria(b.Lens, true) {
		r, ok := b.Criteria[c.Key]
		if !ok {
			continue
		}
		fc := FitCriterion{Key: c.Key, Question: c.Question, Weight: c.Weight, Level: r.Level, Why: r.Why, Phase: r.Phase, Campaign: r.Campaign}
		if r.Level >= 1 && r.Level <= 5 {
			fc.Means = c.Levels[r.Level-1]
		}
		f.Criteria = append(f.Criteria, fc)
	}
	for _, rule := range ideafit.Caps(b.Lens) {
		if slices.Contains(b.Caps, rule.Cap) {
			f.Caps = append(f.Caps, FitCap{Cap: string(rule.Cap), Meaning: rule.Meaning})
		}
	}
	return f
}

// ideaAnalysis is the analysis id of assetID, in brief; nil when it can't be
// read or is gone.
func ideaAnalysis(ctx context.Context, d Deps, assetID, id string) *IdeaAnalysis {
	lenses, err := d.Analyses.ListByAsset(ctx, assetID)
	if err != nil {
		return nil
	}
	for i := range lenses {
		if a := &lenses[i]; a.ID == id {
			return &IdeaAnalysis{ID: a.ID, Lens: lensRef(a), Gist: a.Gist, Summary: clip(a.Summary, ideaAnalysisRunes)}
		}
	}
	return nil
}

// IdeasInCommonInput names the ideas to compare.
type IdeasInCommonInput struct {
	IdeaIDs []string `json:"ideaIds,omitempty" jsonschema:"description=2 to 50 ideas; default the ideas selected on screen"`
}

// IdeasInCommonOutput is what the ideas share; each list holds what two or
// more of them have, the most shared first, with which ideas.
type IdeasInCommonOutput struct {
	Ideas     []CommonIdea  `json:"ideas"`
	Missing   []string      `json:"missing,omitempty"`
	Assets    []SharedValue `json:"assets,omitempty"`
	Campaigns []SharedValue `json:"campaigns,omitempty"`
	Origins   []SharedValue `json:"origins,omitempty"`
	Lenses    []SharedValue `json:"lenses,omitempty"`
	Platforms []SharedValue `json:"platforms,omitempty"`
	PostTypes []SharedValue `json:"postTypes,omitempty"`
	Phases    []SharedValue `json:"phases,omitempty"`
	Terms     []SharedValue `json:"terms,omitempty"`
}

// CommonIdea is one compared idea, with its angle for the model to weigh.
type CommonIdea struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Angle string `json:"angle,omitempty"`
}

// SharedValue is one shared value, labelled where it is an id.
type SharedValue struct {
	Value   string   `json:"value"`
	Label   string   `json:"label,omitempty"`
	IdeaIDs []string `json:"ideaIds"`
}

func ideasInCommon(ctx context.Context, d Deps, in IdeasInCommonInput) (*IdeasInCommonOutput, error) {
	ids := dedupe(in.IdeaIDs)
	if len(ids) < 2 {
		return nil, assistantkit.Refused("compare two or more ideas; select them or pass ideaIds")
	}
	if len(ids) > maxCommonIdeas {
		return nil, assistantkit.Refused(fmt.Sprintf("at most %d ideas at once; narrow the selection", maxCommonIdeas))
	}
	out := &IdeasInCommonOutput{Ideas: []CommonIdea{}}
	var list []models.Idea
	for _, id := range ids {
		idea, err := d.Ideas.Get(ctx, id)
		if err != nil {
			if _, ok := errors.AsType[*assistantkit.Refusal](triageErr(err)); ok {
				out.Missing = append(out.Missing, id)
				continue
			}
			return nil, err
		}
		list = append(list, *idea)
		out.Ideas = append(out.Ideas, CommonIdea{ID: idea.ID, Title: idea.Title, Angle: deref(idea.Angle)})
	}
	if len(list) < 2 {
		return nil, assistantkit.Refused("fewer than two of those ideas exist")
	}
	c := ideas.InCommon(list)
	assetTitles := map[string]string{}
	if d.Assets != nil && len(c.Assets) > 0 {
		var aids []string
		for _, s := range c.Assets {
			aids = append(aids, s.Value)
		}
		if metas, err := d.Assets.ListMeta(ctx, aids); err == nil {
			for _, m := range metas {
				assetTitles[m.ID] = m.Title
			}
		}
	}
	var campaignIDs []models.AssetFacetCount
	for _, s := range slices.Concat(c.Campaigns, c.Lenses) {
		campaignIDs = append(campaignIDs, models.AssetFacetCount{Value: s.Value})
	}
	names := campaignNames(ctx, d, campaignIDs)
	names["workspace"] = "the workspace lens"
	label := func(labels map[string]string) func([]ideas.Shared) []SharedValue {
		return func(list []ideas.Shared) []SharedValue {
			var out []SharedValue
			for _, s := range list {
				out = append(out, SharedValue{Value: s.Value, Label: labels[s.Value], IdeaIDs: s.IdeaIDs})
			}
			return out
		}
	}
	plain := label(nil)
	out.Assets, out.Campaigns, out.Lenses = label(assetTitles)(c.Assets), label(names)(c.Campaigns), label(names)(c.Lenses)
	out.Origins, out.Platforms, out.PostTypes, out.Phases, out.Terms = plain(c.Origins), plain(c.Platforms), plain(c.PostTypes), plain(c.Phases), plain(c.Terms)
	return out, nil
}
