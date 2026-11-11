package packs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_compare"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// What compareAssets reads: at most this many assets, this many passages of
// one without an analysis, and this much text of each.
const (
	compareMaxAssets = 8
	comparePassages  = 6
	compareRunes     = 6000
)

// CompareAssetsInput is the assets to summarise or compare.
type CompareAssetsInput struct {
	AssetIDs []string `json:"assetIds,omitempty" jsonschema:"description=The assets, e.g. the selected ones from getSelection; defaults to the asset open on screen"`
	Mode     string   `json:"mode,omitempty"     jsonschema:"enum=summarise,enum=compare,description=compare (the default for two or more) or summarise"`
	Focus    string   `json:"focus,omitempty"    jsonschema:"description=Only what they say about this, when the user narrows it"`
}

// ComparedAsset is one asset read, lettered in the order given, and where
// its text came from: its analysis for the thread's lens, else its passages.
type ComparedAsset struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Letter string `json:"letter"`
	From   string `json:"from"`
	// analysisID is the analysis read, when one was.
	analysisID string
}

// ComparedClaim is a claim and the assets behind it, by id and letter.
type ComparedClaim struct {
	Text     string   `json:"text"`
	AssetIDs []string `json:"assetIds"`
	Letters  []string `json:"letters"`
}

// CompareAssetsOutput is the summary or the comparison, every claim tied to
// its assets.
type CompareAssetsOutput struct {
	Mode      string          `json:"mode"`
	Assets    []ComparedAsset `json:"assets"`
	Summary   string          `json:"summary,omitempty"`
	Shared    []ComparedClaim `json:"shared"`
	Partial   []ComparedClaim `json:"partial"`
	Conflicts []ComparedClaim `json:"conflicts"`
	Skipped   []ComparedAsset `json:"skipped,omitempty"`
	// Missing are the ids asked for that name no asset.
	Missing   []string               `json:"missing,omitempty"`
	Grounding assistantkit.Grounding `json:"grounding"`
	Note      string                 `json:"note,omitempty"`
}

func compareAssets(ctx context.Context, d Deps, in CompareAssetsInput) (*CompareAssetsOutput, error) {
	ids, mode, err := compareRequest(ctx, in)
	if err != nil {
		return nil, err
	}
	out := &CompareAssetsOutput{Mode: mode, Assets: []ComparedAsset{}, Shared: []ComparedClaim{}, Partial: []ComparedClaim{}, Conflicts: []ComparedClaim{}}
	sources, err := gatherSources(ctx, d, ids, in.Focus, out)
	if err != nil {
		return nil, err
	}
	if len(out.Missing) == len(ids) {
		return nil, assistantkit.Refused("none of those assets exist; get their ids from getSelection or findAssets")
	}
	if len(out.Missing) > 0 {
		out.Note = fmt.Sprintf("These ids name no asset and were left out: %s. Say the selection was incomplete.", strings.Join(out.Missing, ", "))
	}
	if len(sources) == 0 || (mode == asset_compare.ModeCompare && len(sources) < 2) {
		out.Grounding = assistantkit.GroundingFrom(nil)
		out.Note = strings.TrimSpace(out.Note + " There's too little to read: the assets have no analysis for this lens and no passages yet.")
		return out, nil
	}
	res, err := d.Compare(ctx, asset_compare.Request{Mode: mode, Focus: in.Focus, Sources: sources})
	if err != nil {
		return nil, assistantkit.Refused("couldn't read the assets together just now; try again")
	}
	letters := map[string]string{}
	grounded := make([]assistantkit.Source, len(out.Assets))
	for i, a := range out.Assets {
		letters[a.ID] = a.Letter
		grounded[i] = assistantkit.Source{ID: a.ID, Title: a.Title, AnalysisID: a.analysisID}
	}
	claims := func(cs []asset_compare.Claim) []ComparedClaim {
		out := make([]ComparedClaim, len(cs))
		for i, c := range cs {
			out[i] = ComparedClaim{Text: c.Text, AssetIDs: c.AssetIDs, Letters: make([]string, len(c.AssetIDs))}
			for j, id := range c.AssetIDs {
				out[i].Letters[j] = letters[id]
			}
		}
		return out
	}
	out.Summary = res.Summary
	out.Shared, out.Partial, out.Conflicts = claims(res.Shared), claims(res.Partial), claims(res.Conflicts)
	out.Grounding = assistantkit.GroundingFrom(grounded)
	return out, nil
}

// compareRequest resolves the assets — the ones asked for, else the one open
// on screen — and the mode: compare for two or more, else summarise.
func compareRequest(ctx context.Context, in CompareAssetsInput) ([]string, string, error) {
	ids := dedupe(in.AssetIDs)
	if len(ids) == 0 {
		if open := assistant.FocusedAsset(ctx); open != "" {
			ids = []string{open}
		}
	}
	mode := in.Mode
	if mode == "" {
		mode = asset_compare.ModeSummarise
		if len(ids) > 1 {
			mode = asset_compare.ModeCompare
		}
	}
	switch {
	case mode != asset_compare.ModeSummarise && mode != asset_compare.ModeCompare:
		return nil, "", assistantkit.Refused("mode is summarise or compare")
	case len(ids) == 0:
		return nil, "", assistantkit.Refused("which assets? pass their ids; get them from getSelection or findAssets")
	case mode == asset_compare.ModeCompare && len(ids) < 2:
		return nil, "", assistantkit.Refused("comparing needs two assets or more; summarise one instead")
	case len(ids) > compareMaxAssets:
		return nil, "", assistantkit.Refused(fmt.Sprintf("at most %d assets at once; narrow the selection", compareMaxAssets))
	}
	return ids, mode, nil
}

// gatherSources reads each existing asset of ids, in order, lettering the
// ones with something to read into out.Assets and naming the rest in
// out.Skipped.
func gatherSources(ctx context.Context, d Deps, ids []string, focus string, out *CompareAssetsOutput) ([]asset_compare.Source, error) {
	metas, err := d.Assets.ListMeta(ctx, ids)
	if err != nil {
		return nil, err
	}
	titles := map[string]string{}
	for _, a := range metas {
		titles[a.ID] = a.Title
	}
	var sources []asset_compare.Source
	for _, id := range ids {
		title, ok := titles[id]
		if !ok {
			out.Missing = append(out.Missing, id)
			continue
		}
		m, err := material(ctx, d, id, focus)
		if err != nil {
			return nil, err
		}
		a := ComparedAsset{ID: id, Title: title, From: m.from, analysisID: m.analysisID}
		if m.text == "" {
			out.Skipped = append(out.Skipped, a)
			continue
		}
		a.Letter = string(rune('A' + len(out.Assets)))
		out.Assets = append(out.Assets, a)
		sources = append(sources, asset_compare.Source{ID: id, Title: title, From: m.from, Text: m.text})
	}
	return sources, nil
}

// material is what to read of an asset: the summary and key points of its
// analysis for the thread's lens, else its passages. With a focus, the
// passages about it are read too, beside an analysis that may not cover it;
// without one, an asset with no analysis is read from its first passages.
// No text when it has neither.
func material(ctx context.Context, d Deps, assetID, focus string) (assetMaterial, error) {
	focus = strings.TrimSpace(focus)
	about, err := focusedPassages(ctx, d, assetID, focus)
	if err != nil {
		return assetMaterial{}, err
	}
	if d.Analyses != nil {
		lens, err := d.Analyses.GetLens(ctx, assetID, lensCampaign(ctx, ""))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return assetMaterial{}, err
		}
		if lens != nil && lens.Status == models.AssetAnalysisStatusReady && strings.TrimSpace(lens.Summary) != "" {
			text := lens.Summary
			if len(lens.Result.KeyPoints) > 0 {
				text += "\n\nKey points:\n- " + strings.Join(lens.Result.KeyPoints, "\n- ")
			}
			if about != "" {
				text = clip(text, compareRunes/2) + "\n\nPassages about " + focus + ":\n" + about
			}
			return assetMaterial{from: asset_compare.FromAnalysis, text: clip(text, compareRunes), analysisID: lens.ID}, nil
		}
	}
	if about == "" && d.Chunks != nil {
		chunks, _, err := d.Chunks.ListPageByAssetID(ctx, assetID, 0, comparePassages)
		if err != nil {
			return assetMaterial{}, err
		}
		about = joinPassages(chunks)
	}
	return assetMaterial{from: asset_compare.FromPassages, text: clip(about, compareRunes)}, nil
}

// assetMaterial is what is read of an asset, where from, and the analysis
// it came from when it did.
type assetMaterial struct {
	from, text, analysisID string
}

// focusedPassages is the asset's passages about focus; "" without a focus,
// or when search is unavailable or finds none.
func focusedPassages(ctx context.Context, d Deps, assetID, focus string) (string, error) {
	if focus == "" || d.Chunks == nil || !embedopts.Available(d.Embedder) {
		return "", nil
	}
	emb, err := d.Embedder.Embed(ctx, &ai.EmbedRequest{Input: []*ai.Document{ai.DocumentFromText(focus, nil)}, Options: embedopts.Query()})
	if err != nil || len(emb.Embeddings) == 0 {
		return "", nil
	}
	chunks, err := d.Chunks.SearchSimilar(ctx, pgvector.NewHalfVector(emb.Embeddings[0].Embedding), []string{assetID}, askMinSimilarity, comparePassages)
	if err != nil {
		return "", err
	}
	return joinPassages(chunks), nil
}

func joinPassages(chunks []models.AssetChunk) string {
	parts := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if p := pages(c.PageStart, c.PageEnd); p != "" {
			parts = append(parts, "("+p+") "+c.Content)
		} else {
			parts = append(parts, c.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}
