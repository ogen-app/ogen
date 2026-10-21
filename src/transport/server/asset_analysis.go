package server

import (
	"context"
	"fmt"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// newAssetAnalysis builds the analysis service and plugs it into every place
// an asset's text settles: the ingest workers and the markdown embedder. The
// model is bound later, once the Genkit runtime is up (initGenkit).
func newAssetAnalysis(d *deps) *assetanalysis.Service {
	r := d.r
	svc := assetanalysis.New(d.db, assetanalysis.Repos{
		Analyses:  r.assetAnalysisRepo,
		Assets:    r.pieceRepo,
		Chunks:    r.chunksRepo,
		Campaigns: r.campaignRepo,
		Posts:     r.postRepo,
		Ideas:     r.ideaRepo,
		Brands:    r.brandRepo,
		Tenants:   r.tenantRepo,
		Distills:  r.ideaDistillRepo,
	}, d.entitlements.limiter, d.hub)
	svc.SetJobs(d.enqueuer, d.cfg.AnalysisRebriefDebounce, d.cfg.AnalysisEditDebounce)

	d.ingest.pdf.Analysis = svc
	d.ingest.document.Analysis = svc
	d.ingest.audio.Analysis = svc
	d.ingest.url.Analysis = svc
	// The embedder only runs for authored assets (notes, Markdown), which
	// are re-saved as they are typed, so their analysis is debounced.
	flows.SetOnAssetEmbedded(svc.ScheduleEdited)
	return svc
}

// assetAnalysisFlows are the callbacks of the analysis flows.
type assetAnalysisFlows struct {
	analyze func(ctx context.Context, req asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error)
	distill func(ctx context.Context, req asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error)
}

// initAssetAnalysis registers the analyzeAsset and distillAssetIdeas flows
// and returns their callbacks.
func initAssetAnalysis(
	g *genkit.Genkit,
	cfg *config.Config,
	provider *llm.Provider,
	recorder *usage.Recorder,
	checker *usage.Checker,
) (assetAnalysisFlows, error) {
	flowCfg := asset_analysis.FlowConfig{
		Provider:             provider,
		Recorder:             recorder,
		Checker:              checker,
		FullContextMaxTokens: cfg.AnalysisFullContextMaxTokens,
		MaxSourceTokens:      cfg.AnalysisMaxSourceTokens,
	}
	if err := asset_analysis.InitAssetAnalysis(g, flowCfg); err != nil {
		return assetAnalysisFlows{}, fmt.Errorf("init asset analysis flow: %w", err)
	}
	return assetAnalysisFlows{
		analyze: asset_analysis.NewAnalyzeAssetCallback(),
		distill: asset_analysis.NewDistillIdeasCallback(),
	}, nil
}

// monthStart is the start of now's calendar month in UTC, where the monthly
// analysis allowance resets.
func monthStart(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}
