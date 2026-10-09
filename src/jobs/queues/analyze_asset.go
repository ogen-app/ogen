package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// AnalysisQueue is the dedicated River queue analyses run on: a long
// transcript can keep a worker for minutes, which must not hold up ingestion,
// publishing or email.
const AnalysisQueue = "analysis"

const (
	AnalyzeAssetKind      = "analyze_asset"
	ReanalyzeCampaignKind = "reanalyze_campaign"
)

// AnalysisRunner is the slice of the analysis use case the jobs drive.
// *assetanalysis.Service implements it.
type AnalysisRunner interface {
	Run(ctx context.Context, l assetanalysis.Lens) error
	Fail(ctx context.Context, l assetanalysis.Lens, cause error)
	Rebrief(ctx context.Context, tenantID, campaignID string) error
}

// AnalysisDeps are the analysis workers' dependencies. A nil Runner makes
// both jobs no-ops.
type AnalysisDeps struct {
	Runner  AnalysisRunner
	Tenants TenantStatusReader
}

// analysisUniqueStates lets a new job for a lens queue up while one runs, so
// a brief edit made mid-run is analysed again, but folds duplicates that are
// still waiting.
var analysisUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateScheduled,
}

// AnalyzeAssetTask analyses one lens of an asset. Reason only labels the run;
// uniqueness is per lens.
type AnalyzeAssetTask struct {
	TenantID   string  `json:"tenant_id"   river:"unique"`
	AssetID    string  `json:"asset_id"    river:"unique"`
	CampaignID *string `json:"campaign_id" river:"unique"`
	Reason     string  `json:"reason"`
}

func (AnalyzeAssetTask) Kind() string { return AnalyzeAssetKind }

func (AnalyzeAssetTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       AnalysisQueue,
		MaxAttempts: 5,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: analysisUniqueStates},
	}
}

func (t AnalyzeAssetTask) lens() assetanalysis.Lens {
	return assetanalysis.Lens{TenantID: t.TenantID, AssetID: t.AssetID, CampaignID: t.CampaignID}
}

type AnalyzeAssetProcessor struct {
	river.WorkerDefaults[AnalyzeAssetTask]
	Deps AnalysisDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &AnalyzeAssetProcessor{Deps: d.Analysis})
		river.AddWorker(w, &ReanalyzeCampaignProcessor{Deps: d.Analysis})
	})
}

func (p *AnalyzeAssetProcessor) Work(ctx context.Context, job *river.Job[AnalyzeAssetTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout covers a map-reduce run over a source near the token cap.
func (p *AnalyzeAssetProcessor) Timeout(*river.Job[AnalyzeAssetTask]) time.Duration {
	return 15 * time.Minute
}

// Process runs the lens. On the last attempt a failure is stored on the lens
// so it never stays pending.
func (p *AnalyzeAssetProcessor) Process(ctx context.Context, task AnalyzeAssetTask, lastAttempt bool) error {
	if p.Deps.Runner == nil {
		return nil
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	if active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID); err != nil || !active {
		return err
	}
	err := p.Deps.Runner.Run(ctx, task.lens())
	if err != nil && lastAttempt {
		slog.WarnContext(ctx, "analysis failed on its last attempt", logging.AttrComponent, "jobs.analyze_asset",
			"asset_id", task.AssetID, "reason", task.Reason, logging.AttrError, err)
		p.Deps.Runner.Fail(ctx, task.lens(), err)
		return nil
	}
	return err
}

// ReanalyzeCampaignTask re-runs a campaign's lenses after its brief changed.
// It is scheduled a debounce ahead and unique per campaign while waiting, so
// a burst of brief saves collapses into one run.
type ReanalyzeCampaignTask struct {
	TenantID   string `json:"tenant_id"   river:"unique"`
	CampaignID string `json:"campaign_id" river:"unique"`
}

func (ReanalyzeCampaignTask) Kind() string { return ReanalyzeCampaignKind }

func (ReanalyzeCampaignTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       AnalysisQueue,
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateScheduled,
		}},
	}
}

type ReanalyzeCampaignProcessor struct {
	river.WorkerDefaults[ReanalyzeCampaignTask]
	Deps AnalysisDeps
}

func (p *ReanalyzeCampaignProcessor) Work(ctx context.Context, job *river.Job[ReanalyzeCampaignTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args)
}

func (p *ReanalyzeCampaignProcessor) Timeout(*river.Job[ReanalyzeCampaignTask]) time.Duration {
	return time.Minute
}

func (p *ReanalyzeCampaignProcessor) Process(ctx context.Context, task ReanalyzeCampaignTask) error {
	if p.Deps.Runner == nil {
		return nil
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	if active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID); err != nil || !active {
		return err
	}
	return p.Deps.Runner.Rebrief(ctx, task.TenantID, task.CampaignID)
}

// EnqueueAnalyzeAsset queues one lens's analysis. A lens already waiting is
// not queued twice.
func (e *Enqueuer) EnqueueAnalyzeAsset(ctx context.Context, tenantID, assetID string, campaignID *string, reason string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.Insert(ctx, AnalyzeAssetTask{TenantID: tenantID, AssetID: assetID, CampaignID: campaignID, Reason: reason}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueReanalyzeCampaign schedules a campaign's re-analysis at at. While
// one is waiting, further calls are no-ops.
func (e *Enqueuer) EnqueueReanalyzeCampaign(ctx context.Context, tenantID, campaignID string, at time.Time) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.Insert(ctx, ReanalyzeCampaignTask{TenantID: tenantID, CampaignID: campaignID}, insertOptsWithRequestID(ctx, &river.InsertOpts{ScheduledAt: at}))
	return err
}
