package packs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_compare"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type compareLenses struct {
	repository.AssetAnalysisRepository
	ready map[string]string // asset → summary of its lens in scope
	asked []*string
}

func (r *compareLenses) GetLens(_ context.Context, assetID string, campaignID *string) (*models.AssetAnalysis, error) {
	r.asked = append(r.asked, campaignID)
	s, ok := r.ready[assetID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return &models.AssetAnalysis{AssetID: assetID, Status: models.AssetAnalysisStatusReady, Summary: s,
		Result: models.AssetAnalysisResult{KeyPoints: []string{"kp"}}}, nil
}

type firstChunks struct {
	repository.AssetChunksRepository
	by map[string][]models.AssetChunk
}

func (r firstChunks) ListPageByAssetID(_ context.Context, assetID string, _, limit int) ([]models.AssetChunk, int, error) {
	c := r.by[assetID]
	return c[:min(len(c), limit)], len(c), nil
}

func compareDeps(got *asset_compare.Request) Deps {
	return Deps{
		Assets: memAssets{all: []models.Asset{
			{ID: "a1", Title: "Villa Aurora brochure 2026"}, {ID: "a2", Title: "Press kit 2026"},
			{ID: "a3", Title: "Spa menu"}, {ID: "a4", Title: "Empty note"},
		}},
		Analyses: &compareLenses{ready: map[string]string{"a1": "Sells the reopening.", "a3": "Spa hours 10–20."}},
		Chunks: firstChunks{by: map[string][]models.AssetChunk{
			"a2": {{AssetID: "a2", Content: "Reopening 14 March.", PageStart: new(2)}, {AssetID: "a2", Content: "Head chef Marta Ruiz."}},
		}},
		Compare: func(_ context.Context, req asset_compare.Request) (*asset_compare.Result, error) {
			*got = req
			return &asset_compare.Result{
				Shared:    []asset_compare.Claim{{Text: "Reopening 14 March", AssetIDs: []string{"a1", "a2", "a3"}}},
				Partial:   []asset_compare.Claim{{Text: "A new head chef", AssetIDs: []string{"a2"}}},
				Conflicts: []asset_compare.Claim{{Text: "Spa hours 9–21 against 10–20", AssetIDs: []string{"a1", "a3"}}},
			}, nil
		},
	}
}

func TestCompareAssets(t *testing.T) {
	var req asset_compare.Request
	d := compareDeps(&req)
	ctx, _ := turn(t, campaign1, nil)

	out, err := compareAssets(ctx, d, CompareAssetsInput{AssetIDs: []string{"a1", "a2", "a3", "a4", "a1"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != asset_compare.ModeCompare || req.Mode != asset_compare.ModeCompare {
		t.Fatalf("mode = %q / %q; two assets or more compare", out.Mode, req.Mode)
	}
	var froms []string
	for _, s := range req.Sources {
		froms = append(froms, s.ID+":"+s.From)
	}
	if !slices.Equal(froms, []string{"a1:analysis", "a2:passages", "a3:analysis"}) {
		t.Fatalf("sources = %v; analyses first, passages without one", froms)
	}
	if !strings.Contains(req.Sources[0].Text, "Key points:\n- kp") || req.Sources[1].Text != "(p. 2) Reopening 14 March.\n\nHead chef Marta Ruiz." {
		t.Fatalf("texts = %q / %q", req.Sources[0].Text, req.Sources[1].Text)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].ID != "a4" {
		t.Fatalf("skipped = %+v; an asset with nothing to read is named, not sent", out.Skipped)
	}
	if !slices.Equal(out.Conflicts[0].Letters, []string{"A", "C"}) || !slices.Equal(out.Partial[0].Letters, []string{"B"}) {
		t.Fatalf("letters = %+v / %+v", out.Conflicts, out.Partial)
	}
	if out.Grounding.Mode != "grounded" || len(out.Grounding.Sources) != 3 {
		t.Fatalf("grounding = %+v", out.Grounding)
	}
	// The lens read is the thread's campaign's.
	if lenses := d.Analyses.(*compareLenses).asked; lenses[0] == nil || *lenses[0] != "c1" {
		t.Fatalf("lens = %v", lenses)
	}
}

func TestCompareAssets_Refusals(t *testing.T) {
	var req asset_compare.Request
	d := compareDeps(&req)
	ctx, _ := turn(t, workspace, nil)

	for name, in := range map[string]CompareAssetsInput{
		"no assets":        {},
		"compare one":      {AssetIDs: []string{"a1"}, Mode: asset_compare.ModeCompare},
		"an unknown mode":  {AssetIDs: []string{"a1", "a2"}, Mode: "rank"},
		"too many to read": {AssetIDs: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}},
	} {
		_, err := compareAssets(ctx, d, in)
		if err == nil {
			t.Errorf("%s: ran", name)
			continue
		}
		refused(t, err)
	}

	// One asset summarises; nothing readable says so instead of calling the
	// model.
	out, err := compareAssets(ctx, d, CompareAssetsInput{AssetIDs: []string{"a1"}})
	if err != nil || out.Mode != asset_compare.ModeSummarise {
		t.Fatalf("one asset = %+v, %v", out, err)
	}
	req = asset_compare.Request{}
	out, err = compareAssets(ctx, d, CompareAssetsInput{AssetIDs: []string{"a4", "a1"}})
	if err != nil || out.Note == "" || out.Grounding.Mode != "exploratory" || req.Mode != "" {
		t.Fatalf("one readable of two = %+v, %v; want a note and no model call", out, err)
	}

	d.Compare = func(context.Context, asset_compare.Request) (*asset_compare.Result, error) {
		return nil, errors.New("overloaded")
	}
	_, err = compareAssets(ctx, d, CompareAssetsInput{AssetIDs: []string{"a1", "a3"}})
	refused(t, err)
}
