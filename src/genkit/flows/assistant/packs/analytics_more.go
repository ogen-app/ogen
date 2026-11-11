package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/flows/more_like_these"
)

// maxGrounding caps the campaign's other sources offered to the writer.
const maxGrounding = 5

// MoreLikeTheseInput asks for ideas like the examples.
type MoreLikeTheseInput struct {
	PostIDs     []string `json:"postIds,omitempty"     jsonschema:"description=The posts that worked, at most 5; defaults to the posts selected on screen"`
	Count       int      `json:"count,omitempty"       jsonschema:"description=How many new ideas; default 3"`
	PlatformID  string   `json:"platformId,omitempty"  jsonschema:"description=Defaults to the first example's"`
	CampaignID  string   `json:"campaignId,omitempty"  jsonschema:"description=Defaults to the examples' campaign"`
	Instruction string   `json:"instruction,omitempty" jsonschema:"description=Steering for the writer"`
}

// TrailAsset is an asset on an example's trail.
type TrailAsset struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// TrailIdea is an idea an example was made from, and the asset it came from.
type TrailIdea struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Asset *TrailAsset `json:"asset,omitempty"`
}

// TrailStep is how one example came to be: the ideas it was made from and
// the assets it used.
type TrailStep struct {
	PostID string       `json:"postId"`
	Title  string       `json:"title"`
	Ideas  []TrailIdea  `json:"ideas"`
	Assets []TrailAsset `json:"assets"`
	// NoTrail is an example with no ideas or assets recorded: its own text
	// is the only source, and the new ideas cite it.
	NoTrail bool `json:"noTrail,omitempty"`
}

// NewIdea is an idea "more like these" saved.
type NewIdea struct {
	ID            string                `json:"id"`
	Title         string                `json:"title"`
	Angle         string                `json:"angle,omitempty"`
	Rationale     string                `json:"rationale,omitempty"`
	Sources       []string              `json:"sources"`
	GroundingMode *models.GroundingMode `json:"groundingMode,omitempty"`
	// Grounding is the assets the idea cites, each through the trail idea
	// it came by and its analysis, when known.
	Grounding assistantkit.Grounding `json:"grounding"`
}

// MoreLikeTheseOutput is the trail the ideas came from, then the ideas.
type MoreLikeTheseOutput struct {
	Trail []TrailStep `json:"trail"`
	// Grounding is the campaign's other sources the writer could draw on.
	Grounding  []TrailAsset `json:"grounding,omitempty"`
	CampaignID string       `json:"campaignId,omitempty"`
	Ideas      []NewIdea    `json:"ideas"`
}

func moreLikeTheseSpec(g *genkit.Genkit, d Deps) assistantkit.Spec {
	tool := genkit.DefineTool(g, "moreLikeThese",
		"Writes new ideas in the vein of posts that performed well — their angle and format, not their words — from the ideas and assets behind them, and saves them as undated ideas, so nothing reaches the calendar. Returns the trail each example came from, then the ideas. More than a few are offered for the user to confirm first.",
		assistantkit.Gated("moreLikeThese", assistantkit.Gate[MoreLikeTheseInput]{
			Precheck: func(ctx context.Context, in MoreLikeTheseInput) error {
				_, err := examplesOf(ctx, in)
				return err
			},
			Needs: func(_ context.Context, in MoreLikeTheseInput) bool { return overN(d, ideaCount(in)) },
			Summary: func(in MoreLikeTheseInput) string {
				return fmt.Sprintf("Write %s like the top posts", plural(ideaCount(in), "idea", "ideas"))
			},
		}, assistantkit.Wrap(recorded(analyticsPackName, "moreLikeThese", nil, func(ctx context.Context, in MoreLikeTheseInput) (*MoreLikeTheseOutput, error) {
			return moreLikeThese(ctx, d, in)
		}))), ai.WithStrictSchema(false))
	return spec(analyticsPackName, tool, assistantkit.TierProposal,
		`Call this when the user asks for more like the posts that worked — "give me three ideas like the top performers", "more like the selected posts". Pass postIds from topPosts (at most 5), or none for the posts selected on screen, and count. Show the trail first, then the ideas; they are ideas, not posts — draftIdeas turns them into posts when the user wants.`)
}

func ideaCount(in MoreLikeTheseInput) int { return min(cmp.Or(in.Count, 3), 10) }

// examplesOf is the example posts: the ones named, else the ones selected.
func examplesOf(ctx context.Context, in MoreLikeTheseInput) ([]string, error) {
	ids := dedupe(in.PostIDs)
	if len(ids) == 0 {
		ids = dedupe(assistant.Selected(ctx, viewcontext.KindPost))
	}
	if len(ids) == 0 || len(ids) > maxExamples {
		return nil, assistantkit.Refused(fmt.Sprintf("pass 1 to %d example posts, e.g. from topPosts, or select them", maxExamples))
	}
	return ids, nil
}

// trailSources gathers what the examples came from, as the writer's sources:
// each example's ideas with the asset each came from, and the assets it
// used. It records each example's trail on out.
type trailSources struct {
	d       Deps
	sources []more_like_these.Source
	labels  map[string]string
	assetOf map[string]string
	assets  map[string]*TrailAsset
	// analysisOf is each asset's finished analysis the writer read.
	analysisOf map[string]string
}

func (t *trailSources) add(ref, label, title, text, assetID string) {
	if _, ok := t.labels[ref]; ok {
		return
	}
	t.labels[ref] = label
	if assetID != "" {
		t.assetOf[ref] = assetID
	}
	t.sources = append(t.sources, more_like_these.Source{Ref: ref, Title: title, Text: text})
}

// asset reads an asset's title once; nil when it is gone.
func (t *trailSources) asset(ctx context.Context, id string) (*TrailAsset, error) {
	if a, ok := t.assets[id]; ok {
		return a, nil
	}
	var a *TrailAsset
	if t.d.Assets == nil {
		a = &TrailAsset{ID: id, Title: id}
	} else {
		got, err := t.d.Assets.GetByID(ctx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			a = &TrailAsset{ID: id, Title: got.Title}
		}
	}
	t.assets[id] = a
	return a, nil
}

func moreLikeThese(ctx context.Context, d Deps, in MoreLikeTheseInput) (*MoreLikeTheseOutput, error) {
	ids, err := examplesOf(ctx, in)
	if err != nil {
		return nil, err
	}
	t := &trailSources{d: d, labels: map[string]string{}, assetOf: map[string]string{}, assets: map[string]*TrailAsset{}, analysisOf: map[string]string{}}
	out := &MoreLikeTheseOutput{Trail: []TrailStep{}, Ideas: []NewIdea{}}
	var examples []more_like_these.Example
	var posts []*models.Post
	var assetIDs []string
	for _, id := range ids {
		p, err := d.Posts.GetByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, assistantkit.Refused(fmt.Sprintf("there is no post %q", id))
		}
		if err != nil {
			return nil, err
		}
		posts = append(posts, p)
		examples = append(examples, more_like_these.Example{ID: p.ID, Title: p.Title, Platform: p.PlatformID, Text: p.Content})
		t.labels["post:"+p.ID] = "Post: " + p.Title
		step, err := trailOf(ctx, t, p)
		if err != nil {
			return nil, err
		}
		for _, a := range step.Assets {
			assetIDs = append(assetIDs, a.ID)
		}
		for _, i := range step.Ideas {
			if i.Asset != nil {
				assetIDs = append(assetIDs, i.Asset.ID)
			}
		}
		out.Trail = append(out.Trail, step)
	}

	campaignID := cmp.Or(in.CampaignID, posts[0].CampaignID)
	var campaign *models.Campaign
	if campaignID != "" {
		if campaign, err = d.loadCampaign(ctx, CampaignRef{CampaignID: campaignID}); err != nil {
			return nil, err
		}
		out.CampaignID = campaign.ID
		if out.Grounding, err = groundingOf(ctx, t, campaign, assetIDs); err != nil {
			return nil, err
		}
		for _, a := range out.Grounding {
			assetIDs = append(assetIDs, a.ID)
		}
	}
	if err := t.gists(ctx, assetIDs); err != nil {
		return nil, err
	}

	written, err := d.MoreLike(ctx, more_like_these.Request{Examples: examples, Sources: t.sources, Count: ideaCount(in), Instruction: in.Instruction})
	if err != nil {
		return nil, fmt.Errorf("write ideas: %w", err)
	}
	platform := cmp.Or(in.PlatformID, posts[0].PlatformID)
	for _, w := range written {
		idea, grounding, err := saveIdea(ctx, d, t, w, campaign, platform, ids)
		if err != nil {
			return nil, err
		}
		out.Ideas = append(out.Ideas, NewIdea{ID: idea.ID, Title: idea.Title, Angle: w.Angle, Rationale: w.Rationale,
			Sources: idea.Sources, GroundingMode: idea.GroundingMode, Grounding: grounding})
		assistantkit.Touch(ctx, "idea", idea.ID)
	}
	return out, nil
}

// trailOf is one example's trail: its ideas, the asset each came from, and
// the assets it used, each a source for the writer.
func trailOf(ctx context.Context, t *trailSources, p *models.Post) (TrailStep, error) {
	step := TrailStep{PostID: p.ID, Title: p.Title, Ideas: []TrailIdea{}, Assets: []TrailAsset{}}
	linked, err := t.d.Links.IdeasOfPost(ctx, p.ID)
	if err != nil {
		return step, err
	}
	for _, li := range linked {
		ti := TrailIdea{ID: li.ID, Title: li.Title}
		idea, err := t.d.IdeaRepo.GetByID(ctx, li.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return step, err
		}
		text, assetID := "", ""
		if idea != nil {
			text = strings.TrimSpace(cmp.Or(deref(idea.Angle), idea.Note))
			assetID = deref(idea.SourceAssetID)
			if assetID == "" && len(idea.AssetIDs) > 0 {
				assetID = idea.AssetIDs[0]
			}
			// An idea distilled from the asset names the analysis it came by.
			if an := deref(idea.AssetAnalysisID); an != "" && assetID == deref(idea.SourceAssetID) {
				t.analysisOf[assetID] = an
			}
		}
		if assetID != "" {
			if ti.Asset, err = t.asset(ctx, assetID); err != nil {
				return step, err
			}
			if ti.Asset == nil {
				assetID = ""
			}
		}
		step.Ideas = append(step.Ideas, ti)
		t.add("idea:"+li.ID, "Idea: "+li.Title, li.Title, text, assetID)
	}
	for _, id := range p.UsedAssetIDs {
		a, err := t.asset(ctx, id)
		if err != nil {
			return step, err
		}
		if a == nil {
			continue
		}
		step.Assets = append(step.Assets, *a)
		t.add("asset:"+a.ID, "Asset: "+a.Title, a.Title, "", a.ID)
	}
	step.NoTrail = len(step.Ideas) == 0 && len(step.Assets) == 0
	return step, nil
}

// groundingOf is the campaign's ready sources not already on the trail, up
// to maxGrounding, each a source for the writer.
func groundingOf(ctx context.Context, t *trailSources, c *models.Campaign, onTrail []string) ([]TrailAsset, error) {
	if t.d.Assets == nil {
		return nil, nil
	}
	ready, _, err := flowkit.ReadyCampaignAssetIDs(ctx, t.d.Assets, c)
	if err != nil {
		return nil, fmt.Errorf("read the campaign's sources: %w", err)
	}
	var out []TrailAsset
	for _, id := range ready {
		if len(out) == maxGrounding {
			break
		}
		if slices.Contains(onTrail, id) {
			continue
		}
		a, err := t.asset(ctx, id)
		if err != nil {
			return nil, err
		}
		if a == nil {
			continue
		}
		out = append(out, *a)
		t.add("asset:"+a.ID, "Asset: "+a.Title, a.Title, "", a.ID)
	}
	return out, nil
}

// gists fills the assets' sources with what their analysis says they offer.
func (t *trailSources) gists(ctx context.Context, assetIDs []string) error {
	if t.d.Analyses == nil || len(assetIDs) == 0 {
		return nil
	}
	digests, err := t.d.Analyses.LensDigests(ctx, dedupe(assetIDs), nil)
	if err != nil {
		return fmt.Errorf("read the sources' analyses: %w", err)
	}
	for i, s := range t.sources {
		id, ok := strings.CutPrefix(s.Ref, "asset:")
		if !ok {
			continue
		}
		if dg, ok := digests[id]; ok {
			t.sources[i].Text = cmp.Or(dg.Gist, dg.SummaryHead)
			if dg.Status == models.AssetAnalysisStatusReady {
				t.analysisOf[id] = dg.ID
			}
		}
	}
	return nil
}

// saveIdea saves one written idea, undated, in the campaign: its sources by
// name, the assets it cites, and whether those ground it. An idea citing
// nothing cites the examples, its only source. It returns the idea's
// grounding too.
func saveIdea(ctx context.Context, d Deps, t *trailSources, w more_like_these.Idea, c *models.Campaign, platform string, examples []string) (*models.Idea, assistantkit.Grounding, error) {
	refs := w.Sources
	if len(refs) == 0 {
		for _, id := range examples {
			refs = append(refs, "post:"+id)
		}
	}
	id, err := models.NewID()
	if err != nil {
		return nil, assistantkit.Grounding{}, err
	}
	idea := &models.Idea{ID: id, Title: w.Title, Origin: models.IdeaOriginAssistant, Sources: []string{}, AssetIDs: []string{}}
	var sources []assistantkit.Source
	for _, r := range refs {
		if label, ok := t.labels[r]; ok {
			idea.Sources = append(idea.Sources, label)
		}
		if a := t.assetOf[r]; a != "" && !slices.Contains(idea.AssetIDs, a) {
			idea.AssetIDs = append(idea.AssetIDs, a)
			src := assistantkit.Source{ID: a, AnalysisID: t.analysisOf[a]}
			if ta := t.assets[a]; ta != nil {
				src.Title = ta.Title
			}
			if idea, ok := strings.CutPrefix(r, "idea:"); ok {
				src.IdeaID = idea
			}
			sources = append(sources, src)
		}
	}
	idea.GroundingMode = models.GroundingFor(len(idea.AssetIDs))
	if len(idea.AssetIDs) > 0 {
		idea.SourceAssetID = &idea.AssetIDs[0]
	}
	if w.Angle != "" {
		idea.Angle = &w.Angle
	}
	if w.Rationale != "" {
		idea.Rationale = &w.Rationale
	}
	var note []string
	for _, th := range w.Theses {
		note = append(note, "- "+th)
	}
	idea.Note = strings.Join(note, "\n")
	if c != nil {
		idea.CampaignID = &c.ID
	}
	if platform != "" {
		idea.Plan.PlatformID = &platform
	}
	if by := actor(ctx); by != "" {
		idea.CreatedBy = &by
	}
	if err := d.IdeaRepo.Create(ctx, idea); err != nil {
		return nil, assistantkit.Grounding{}, fmt.Errorf("save idea: %w", err)
	}
	return idea, assistantkit.GroundingFrom(sources), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
