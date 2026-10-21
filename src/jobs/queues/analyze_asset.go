package queues

import (
	"cmp"
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
	AnalyzeAssetKind          = "analyze_asset"
	ReanalyzeCampaignKind     = "reanalyze_campaign"
	BackfillAssetAnalysesKind = "backfill_asset_analyses"
)

// DefaultBackfillBatch is how many missing lenses a backfill tick queues per
// tenant when AnalysisDeps.BackfillBatch is unset.
const DefaultBackfillBatch = 20

// AnalysisRunner is the slice of the analysis use case the jobs drive.
// *assetanalysis.Service implements it.
type AnalysisRunner interface {
	Run(ctx context.Context, l assetanalysis.Lens, reason string) error
	Fail(ctx context.Context, l assetanalysis.Lens, cause error)
	Rebrief(ctx context.Context, tenantID, campaignID string) error
	EditWait(ctx context.Context, l assetanalysis.Lens, since time.Time) (time.Duration, bool)
	Backfill(ctx context.Context, perTenant int) (int, error)
}

// AnalysisDeps are the analysis workers' dependencies. A nil Runner makes
// every analysis job a no-op.
type AnalysisDeps struct {
	Runner AnalysisRunner
	// Distiller runs distill jobs; nil makes them no-ops.
	Distiller DistillRunner
	Tenants   TenantStatusReader
	// BackfillBatch caps the lenses one backfill tick queues per tenant, so
	// one large library cannot fill the analysis queue.
	BackfillBatch int
}

// analysisUniqueStates folds a new job for a lens into one that is waiting or
// running. River requires running in any explicit ByState, so a save made
// mid-run is not queued; the running job re-runs itself instead when the
// asset changed under it (see Process).
var analysisUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
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
		river.AddWorker(w, &BackfillAssetAnalysesProcessor{Deps: d.Analysis})
		river.AddWorker(w, &DistillAssetIdeasProcessor{Deps: d.Analysis})
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

// Process runs the lens. An edit-triggered run snoozes until the asset has
// been quiet for the whole edit debounce; snoozing uses no attempt, and the
// snoozed job still folds in later saves. A run whose asset changed while it
// ran snoozes the same way to read the new text, since the saves it raced
// were folded into it. On the last attempt a failure is stored on the lens so
// it never stays pending.
func (p *AnalyzeAssetProcessor) Process(ctx context.Context, task AnalyzeAssetTask, lastAttempt bool) error {
	if p.Deps.Runner == nil {
		return nil
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	if active, err := tenantIsActive(ctx, p.Deps.Tenants, task.TenantID); err != nil || !active {
		return err
	}
	if task.Reason == assetanalysis.ReasonEdited {
		if wait, _ := p.Deps.Runner.EditWait(ctx, task.lens(), time.Time{}); wait > 0 {
			return river.JobSnooze(wait)
		}
	}
	started := time.Now()
	err := p.Deps.Runner.Run(ctx, task.lens(), task.Reason)
	if err == nil {
		if wait, changed := p.Deps.Runner.EditWait(ctx, task.lens(), started); changed {
			return river.JobSnooze(wait)
		}
	}
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
			rivertype.JobStateRunning,
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

// BackfillAssetAnalysesTask is the periodic sweep that queues the lenses no
// trigger made: assets and memberships from before analysis existed, and any
// a best-effort trigger dropped. A marker payload.
type BackfillAssetAnalysesTask struct{}

func (BackfillAssetAnalysesTask) Kind() string { return BackfillAssetAnalysesKind }

// InsertOpts: one attempt, since the next tick picks up whatever this one
// missed, and active-state uniqueness so overlapping ticks don't stack.
func (BackfillAssetAnalysesTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: AnalysisQueue, MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

type BackfillAssetAnalysesProcessor struct {
	river.WorkerDefaults[BackfillAssetAnalysesTask]
	Deps AnalysisDeps
}

func (p *BackfillAssetAnalysesProcessor) Work(ctx context.Context, job *river.Job[BackfillAssetAnalysesTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx)
}

func (p *BackfillAssetAnalysesProcessor) Timeout(*river.Job[BackfillAssetAnalysesTask]) time.Duration {
	return 5 * time.Minute
}

// Process queues one batch per tenant. Inactive tenants are left out by the
// query. A failure is logged, not returned: the next tick retries.
func (p *BackfillAssetAnalysesProcessor) Process(ctx context.Context) error {
	if p.Deps.Runner == nil {
		return nil
	}
	queued, err := p.Deps.Runner.Backfill(ctx, cmp.Or(p.Deps.BackfillBatch, DefaultBackfillBatch))
	if err != nil {
		slog.ErrorContext(ctx, "asset analysis backfill failed", logging.AttrComponent, "jobs.backfill_asset_analyses",
			"queued", queued, logging.AttrError, err)
		return nil
	}
	if queued > 0 {
		slog.InfoContext(ctx, "asset analysis backfill queued lenses", logging.AttrComponent, "jobs.backfill_asset_analyses",
			"queued", queued)
	}
	return nil
}

// EnqueueAnalyzeAsset queues one lens's analysis to run at at (now when at is
// zero). A lens already waiting is not queued twice: the waiting job keeps
// its own run time.
func (e *Enqueuer) EnqueueAnalyzeAsset(ctx context.Context, tenantID, assetID string, campaignID *string, reason string, at time.Time) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.Insert(ctx, AnalyzeAssetTask{TenantID: tenantID, AssetID: assetID, CampaignID: campaignID, Reason: reason}, insertOptsWithRequestID(ctx, &river.InsertOpts{ScheduledAt: at}))
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
