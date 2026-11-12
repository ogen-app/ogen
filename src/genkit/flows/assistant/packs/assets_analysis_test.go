package packs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type lensRepo struct {
	repository.AssetAnalysisRepository
	lenses []models.AssetAnalysis
}

func (r lensRepo) ListByAsset(_ context.Context, assetID string) ([]models.AssetAnalysis, error) {
	var out []models.AssetAnalysis
	for _, l := range r.lenses {
		if l.AssetID == assetID {
			out = append(out, l)
		}
	}
	return out, nil
}

type assetIdeas struct {
	repository.IdeaRepository
	ideas []models.Idea
}

func (r assetIdeas) List(_ context.Context, f repository.IdeaListFilter) ([]models.Idea, error) {
	var out []models.Idea
	for _, i := range r.ideas {
		if i.SourceAssetID != nil && *i.SourceAssetID == f.AssetID {
			out = append(out, i)
		}
	}
	return out, nil
}

type assetFiles struct {
	repository.AssetFileRepository
	files map[string]*models.AssetFile
}

func (r assetFiles) ListByAssetIDs(_ context.Context, ids []string) (map[string]*models.AssetFile, error) {
	out := map[string]*models.AssetFile{}
	for _, id := range ids {
		if f, ok := r.files[id]; ok {
			out[id] = f
		}
	}
	return out, nil
}

func analysisDeps() Deps {
	read := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	idea := func(id, lens string, verdict *models.IdeaVerdict, remind *time.Time, fit int) models.Idea {
		return models.Idea{ID: id, Title: "Idea " + id, AssetAnalysisID: new(lens), SourceAssetID: new("a1"),
			Verdict: verdict, RemindAt: remind, FitScore: new(fit)}
	}
	return Deps{
		Assets: memAssets{all: []models.Asset{
			{ID: "a1", Title: "Villa Aurora brochure 2026", Type: new(models.AssetTypePDF)},
			{ID: "a2", Title: "Founder interview", Type: new(models.AssetTypeAudio)},
		}},
		Files: assetFiles{files: map[string]*models.AssetFile{
			"a1": {AssetID: "a1", PageCount: new(14)},
			"a2": {AssetID: "a2", DurationMs: 18 * 60_000},
		}},
		Analyses: lensRepo{lenses: []models.AssetAnalysis{
			{ID: "ws", AssetID: "a1", Status: models.AssetAnalysisStatusReady, AnalyzedAt: &read,
				Gist: "A place to slow down.", Summary: strings.Repeat("s", analysisSummaryRunes+50),
				Result: models.AssetAnalysisResult{KeyPoints: []string{"spa", "terrace"}}},
			{ID: "spring", AssetID: "a1", CampaignID: new("c1"), CampaignName: new("Spring reopening"), Status: models.AssetAnalysisStatusReady, AnalyzedAt: &read},
			{ID: "a2ws", AssetID: "a2", Status: models.AssetAnalysisStatusFailed, FailureReason: "audio too short"},
		}},
		IdeaRepo: assetIdeas{ideas: []models.Idea{
			idea("low", "ws", nil, nil, 20),
			idea("high", "ws", nil, nil, 90),
			idea("woken", "ws", new(models.IdeaVerdictLater), &past, 50),
			idea("snoozed", "ws", new(models.IdeaVerdictLater), &future, 50),
			idea("taken", "ws", new(models.IdeaVerdictYes), nil, 99),
			idea("dropped", "ws", new(models.IdeaVerdictNo), nil, 10),
			idea("spring-1", "spring", nil, nil, 70),
		}},
	}
}

func TestReadAnalysis(t *testing.T) {
	d := analysisDeps()
	ctx, _ := turn(t, workspace, nil)

	out, err := readAnalysis(ctx, d, ReadAnalysisInput{AssetID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Title != "Villa Aurora brochure 2026" || out.Lens.CampaignID != "" || out.Lens.Status != "ready" || out.Lens.ReadAt == nil {
		t.Fatalf("lens = %+v", out)
	}
	if out.Ideas != (IdeaCounts{Undecided: 3, Yes: 1, Later: 1, No: 1}) {
		t.Fatalf("ideas = %+v; a later idea past its reminder is undecided", out.Ideas)
	}
	var order []string
	for _, i := range out.TopIdeas {
		order = append(order, i.ID)
	}
	if !slices.Equal(order, []string{"high", "woken", "low", "taken", "snoozed", "dropped"}) {
		t.Fatalf("top ideas = %v; want undecided first, best fit first", order)
	}
	if len([]rune(out.Summary)) != analysisSummaryRunes || !strings.HasSuffix(out.Summary, "…") {
		t.Fatalf("summary is %d runes", len([]rune(out.Summary)))
	}
	if len(out.OtherLenses) != 1 || out.OtherLenses[0].CampaignName != "Spring reopening" || out.OtherLenses[0].IdeaCount != 1 {
		t.Fatalf("other lenses = %+v; the campaign lens is offered with its idea count, not mixed in", out.OtherLenses)
	}
	if *out.Step != (assistantkit.Step{Workspace: true}) {
		t.Fatalf("step = %+v; want the workspace lens", out.Step)
	}
	if out.Lens.IdeaCount != 6 || out.Type != models.AssetTypePDF || out.PageCount == nil || *out.PageCount != 14 || out.DurationMs != 0 {
		t.Fatalf("lens ideas %d, asset %q, %v pages, %d ms; want 6 ideas and a 14-page PDF", out.Lens.IdeaCount, out.Type, out.PageCount, out.DurationMs)
	}

	// A lens named by campaign, and the thread's campaign by default.
	if out, _ := readAnalysis(ctx, d, ReadAnalysisInput{AssetID: "a1", CampaignID: "c1"}); out.Lens.CampaignName != "Spring reopening" || out.Ideas.Undecided != 1 ||
		out.Step.Subject != "Spring reopening" || out.Step.Workspace {
		t.Fatalf("named lens = %+v", out)
	}
	camp, _ := turn(t, campaign1, nil)
	if out, _ := readAnalysis(camp, d, ReadAnalysisInput{AssetID: "a1"}); out.Lens.CampaignID != "c1" {
		t.Fatalf("thread lens = %+v", out.Lens)
	}
	if out, _ := readAnalysis(camp, d, ReadAnalysisInput{AssetID: "a1", Workspace: true}); out.Lens.CampaignID != "" {
		t.Fatalf("workspace in a campaign thread = %+v", out.Lens)
	}
}

func TestReadAnalysis_NoReading(t *testing.T) {
	d := analysisDeps()
	ctx, _ := turn(t, workspace, nil)

	out, err := readAnalysis(ctx, d, ReadAnalysisInput{AssetID: "a2", CampaignID: "c1"})
	if err != nil || out.Lens.Status != lensNotRead || out.Lens.CampaignID != "c1" || out.Summary != "" || out.Note == "" {
		t.Fatalf("missing lens = %+v, %v; want not_read and nothing from another lens", out, err)
	}
	if len(out.OtherLenses) != 1 || out.OtherLenses[0].Status != "failed" || out.DurationMs != 18*60_000 || out.PageCount != nil {
		t.Fatalf("other lenses = %+v, %d ms; want the recording's length", out.OtherLenses, out.DurationMs)
	}
	out, err = readAnalysis(ctx, d, ReadAnalysisInput{AssetID: "a2"})
	if err != nil || out.Lens.Status != "failed" || out.FailureReason != "audio too short" || len(out.TopIdeas) != 0 {
		t.Fatalf("failed lens = %+v, %v", out, err)
	}

	_, err = readAnalysis(ctx, d, ReadAnalysisInput{})
	refused(t, err)
	_, err = readAnalysis(ctx, d, ReadAnalysisInput{AssetID: "nope"})
	refused(t, err)

	// On an asset page the open asset is the default.
	page, _ := assistant.NewViewTurnContext(t.Context(), viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "asset", Scope: workspace,
		Focus: &viewcontext.Ref{Kind: viewcontext.KindAsset, ID: "a1"},
	}, nil)
	if out, err := readAnalysis(page, d, ReadAnalysisInput{}); err != nil || out.AssetID != "a1" {
		t.Fatalf("open asset = %+v, %v", out, err)
	}
}
