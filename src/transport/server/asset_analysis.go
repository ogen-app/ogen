package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
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
		// Raw table writes, scoped by the tenant in the context.
		DistillSlots: repository.NewIdeaDistillRunSlots(d.db),
	}, d.entitlements.limiter, d.hub)
	svc.SetJobs(d.enqueuer, d.cfg.AnalysisRebriefDebounce, d.cfg.AnalysisEditDebounce)

	settled := sourceSettled{AnalysisScheduler: svc, ready: sourceReady(d)}
	d.ingest.pdf.Analysis = settled
	d.ingest.document.Analysis = settled
	d.ingest.audio.Analysis = settled
	d.ingest.url.Analysis = settled
	// The embedder only runs for authored assets (notes, Markdown), which
	// are re-saved as they are typed, so their analysis is debounced.
	flows.SetOnAssetEmbedded(func(ctx context.Context, tenantID, assetID string) {
		svc.ScheduleEdited(ctx, tenantID, assetID)
		settled.ready(ctx, tenantID, assetID)
	})
	return svc
}

// sourceSettled queues an asset's analyses once its text settles, and tells
// the assistant the asset is ready, for the requests waiting on a source.
type sourceSettled struct {
	queues.AnalysisScheduler
	ready func(ctx context.Context, tenantID, assetID string)
}

func (s sourceSettled) ScheduleAsset(ctx context.Context, tenantID, assetID string) {
	s.AnalysisScheduler.ScheduleAsset(ctx, tenantID, assetID)
	s.ready(ctx, tenantID, assetID)
}

// sourceReady offers again, in the thread of each campaign holding a ready
// asset, the requests that waited for a source. Best-effort: a failure is
// logged, and the ingest it runs in goes on.
func sourceReady(d *deps) func(ctx context.Context, tenantID, assetID string) {
	return func(ctx context.Context, tenantID, assetID string) {
		if d.genkit == nil {
			return
		}
		ctx = tenantctx.With(ctx, tenantID)
		asset, err := d.r.pieceRepo.GetByID(ctx, assetID)
		if err != nil || !models.IsAnalyzableAssetStatus(asset.Status) {
			return
		}
		campaignIDs, err := d.r.assetAnalysisRepo.CampaignIDsWithAsset(ctx, assetID)
		if err != nil {
			slog.WarnContext(ctx, "list the ready source's campaigns failed", logging.AttrComponent, "assistant", "asset_id", assetID, logging.AttrError, err)
			return
		}
		for _, id := range campaignIDs {
			if err := d.genkit.SourceReady(ctx, id, asset.Title); err != nil {
				slog.WarnContext(ctx, "offer the requests waiting on a source failed", logging.AttrComponent, "assistant", "campaign_id", id, "asset_id", assetID, logging.AttrError, err)
			}
		}
	}
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
