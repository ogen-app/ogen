package assetanalysis

import (
	"context"
	"database/sql"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Reasons a lens is (re)analysed; they label jobs and logs.
const (
	ReasonIngested = "ingested"
	ReasonAttached = "attached"
	ReasonBrief    = "brief_changed"
	ReasonRetry    = "retry"
	ReasonEdited   = "edited"
	ReasonBackfill = "backfill"
)

// DefaultRebriefDebounce is how long a brief edit waits before its campaign's
// lenses re-run, so a burst of saves costs one run.
const DefaultRebriefDebounce = 10 * time.Minute

// DefaultEditDebounce is how long an authored asset must sit unedited before
// its lenses re-run, so typing in a note costs one run, not one per save.
const DefaultEditDebounce = 5 * time.Minute

var (
	debouncedTotal        = expvar.NewInt("ogen_asset_analysis_debounced_total")
	backfillEnqueuedTotal = expvar.NewInt("ogen_asset_analysis_backfill_enqueued_total")
	retryCreatedTotal     = expvar.NewInt("ogen_asset_analysis_retry_created_total")
)

// Jobs enqueues the analysis jobs. *queues.Enqueuer implements it.
type Jobs interface {
	// EnqueueAnalyzeAsset queues one lens's analysis to run at at; a zero at
	// runs it now.
	EnqueueAnalyzeAsset(ctx context.Context, tenantID, assetID string, campaignID *string, reason string, at time.Time) error
	EnqueueReanalyzeCampaign(ctx context.Context, tenantID, campaignID string, at time.Time) error
	EnqueueDistillIdeas(ctx context.Context, tenantID, runID string) error
}

// ErrNotRetryable means the lens is neither failed nor skipped.
var ErrNotRetryable = errors.New("analysis is not failed or skipped")

// ErrNotAnalyzable means the asset type is never analysed.
var ErrNotAnalyzable = errors.New("asset type is not analysed")

// SetJobs binds the job queue and the brief-edit and asset-edit debounces;
// a non-positive debounce takes its default.
func (s *Service) SetJobs(jobs Jobs, rebriefDebounce, editDebounce time.Duration) {
	s.jobs = jobs
	s.rebriefDebounce = rebriefDebounce
	if s.rebriefDebounce <= 0 {
		s.rebriefDebounce = DefaultRebriefDebounce
	}
	s.editDebounce = editDebounce
	if s.editDebounce <= 0 {
		s.editDebounce = DefaultEditDebounce
	}
}

// ScheduleAsset queues every lens of an asset whose text has just been
// (re)ingested: the workspace lens and one per campaign holding it.
func (s *Service) ScheduleAsset(ctx context.Context, tenantID, assetID string) {
	ctx = tenantctx.With(ctx, tenantID)
	asset, err := s.repos.Assets.GetByID(ctx, assetID)
	if err != nil || !models.IsAnalyzableAssetType(asset.Type) || !models.IsAnalyzableAssetStatus(asset.Status) {
		return
	}
	campaignIDs, err := s.repos.Analyses.CampaignIDsWithAsset(ctx, assetID)
	if err != nil {
		s.warn(ctx, "list the asset's campaigns", assetID, err)
	}
	s.enqueue(ctx, Lens{TenantID: tenantID, AssetID: assetID}, ReasonIngested)
	for _, id := range campaignIDs {
		s.enqueue(ctx, Lens{TenantID: tenantID, AssetID: assetID, CampaignID: new(id)}, ReasonIngested)
	}
}

// ScheduleEdited queues every lens of an authored asset (a note or Markdown
// file) whose text was just saved, an edit debounce ahead. The lens rows are
// left alone, so a ready lens keeps its reading until the run starts. Saves
// while a job waits fold into it, and the job snoozes until the asset has sat
// unedited for the whole debounce (EditWait), so the debounce is trailing.
func (s *Service) ScheduleEdited(ctx context.Context, tenantID, assetID string) {
	if s.jobs == nil {
		return
	}
	ctx = tenantctx.With(ctx, tenantID)
	asset, err := s.repos.Assets.GetByID(ctx, assetID)
	if err != nil || !models.IsAnalyzableAssetType(asset.Type) || !models.IsAnalyzableAssetStatus(asset.Status) {
		return
	}
	campaignIDs, err := s.repos.Analyses.CampaignIDsWithAsset(ctx, assetID)
	if err != nil {
		s.warn(ctx, "list the asset's campaigns", assetID, err)
	}
	at := s.now().Add(s.editDebounce)
	lenses := []*string{nil}
	for _, id := range campaignIDs {
		lenses = append(lenses, new(id))
	}
	for _, campaignID := range lenses {
		if err := s.jobs.EnqueueAnalyzeAsset(ctx, tenantID, assetID, campaignID, ReasonEdited, at); err != nil {
			s.warn(ctx, "queue the debounced analysis", assetID, err)
			continue
		}
		debouncedTotal.Add(1)
	}
}

// EditWait reports whether the asset changed after since and, if so, how
// much longer its analysis should wait: the rest of the edit debounce since
// that change, zero once it has been quiet that long. A deleted asset never
// changed.
func (s *Service) EditWait(ctx context.Context, l Lens, since time.Time) (time.Duration, bool) {
	ctx = tenantctx.With(ctx, l.TenantID)
	asset, err := s.repos.Assets.GetByID(ctx, l.AssetID)
	if err != nil || !asset.UpdatedAt.After(since) {
		return 0, false
	}
	return max(asset.UpdatedAt.Add(s.editDebounce).Sub(s.now()), 0), true
}

// ScheduleAttached queues the campaign lens of each newly attached asset
// that is already analysable. One still being ingested gets its lens when
// ingestion completes.
func (s *Service) ScheduleAttached(ctx context.Context, tenantID, campaignID string, assetIDs []string) {
	if len(assetIDs) == 0 {
		return
	}
	ctx = tenantctx.With(ctx, tenantID)
	assets, err := s.repos.Assets.ListMeta(ctx, assetIDs)
	if err != nil {
		s.warn(ctx, "load attached assets", campaignID, err)
		return
	}
	for _, a := range assets {
		if models.IsAnalyzableAssetType(a.Type) && models.IsAnalyzableAssetStatus(a.Status) {
			s.enqueue(ctx, Lens{TenantID: tenantID, AssetID: a.ID, CampaignID: new(campaignID)}, ReasonAttached)
		}
	}
}

// Detach drops the campaign lens of each detached asset with its inbox
// ideas; triaged ideas stay.
func (s *Service) Detach(ctx context.Context, tenantID, campaignID string, assetIDs []string) {
	ctx = tenantctx.With(ctx, tenantID)
	for _, assetID := range assetIDs {
		if err := s.repos.Analyses.DeleteLens(ctx, s.db, assetID, campaignID); err != nil {
			s.warn(ctx, "drop the detached lens", assetID, err)
		}
	}
}

// ScheduleRebrief queues a debounced re-run of a campaign's lenses after its
// brief changed. Saves within the debounce window collapse into one run.
func (s *Service) ScheduleRebrief(ctx context.Context, tenantID, campaignID string) {
	if s.jobs == nil {
		return
	}
	if err := s.jobs.EnqueueReanalyzeCampaign(ctx, tenantID, campaignID, s.now().Add(s.rebriefDebounce)); err != nil {
		s.warn(ctx, "queue the campaign re-analysis", campaignID, err)
	}
}

// Rebrief queues every lens of the campaign. Lenses whose inputs did not
// change finish without a model call.
func (s *Service) Rebrief(ctx context.Context, tenantID, campaignID string) error {
	ctx = tenantctx.With(ctx, tenantID)
	lenses, err := s.repos.Analyses.ListByCampaign(ctx, campaignID)
	if err != nil {
		return fmt.Errorf("list campaign lenses: %w", err)
	}
	for _, l := range lenses {
		s.enqueue(ctx, Lens{TenantID: tenantID, AssetID: l.AssetID, CampaignID: l.CampaignID}, ReasonBrief)
	}
	return nil
}

// Retry re-queues a failed or skipped lens, or starts a lens no trigger ever
// made. Starting one needs an analysable asset (else ErrNotRetryable) and,
// for a campaign lens, an asset the campaign holds (else sql.ErrNoRows).
func (s *Service) Retry(ctx context.Context, tenantID, assetID string, campaignID *string) error {
	ctx = tenantctx.With(ctx, tenantID)
	row, err := s.repos.Analyses.GetLens(ctx, assetID, campaignID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.start(ctx, Lens{TenantID: tenantID, AssetID: assetID, CampaignID: campaignID})
	}
	if err != nil {
		return err
	}
	if !row.Status.Retryable() {
		return ErrNotRetryable
	}
	if s.jobs == nil {
		return ErrUnavailable
	}
	if err := s.repos.Analyses.SetStatus(ctx, s.db, row.ID, models.AssetAnalysisStatusPending, "", ""); err != nil {
		return err
	}
	l := Lens{TenantID: tenantID, AssetID: assetID, CampaignID: campaignID}
	s.publish(ctx, l, row.ID, models.AssetAnalysisStatusPending, 0)
	return s.jobs.EnqueueAnalyzeAsset(ctx, tenantID, assetID, campaignID, ReasonRetry, time.Time{})
}

// start creates a missing lens pending and queues it.
func (s *Service) start(ctx context.Context, l Lens) error {
	asset, err := s.repos.Assets.GetByID(ctx, l.AssetID)
	if err != nil {
		return err
	}
	if !models.IsAnalyzableAssetType(asset.Type) || !models.IsAnalyzableAssetStatus(asset.Status) {
		return ErrNotRetryable
	}
	if l.CampaignID != nil {
		campaign, err := s.repos.Campaigns.GetByID(ctx, *l.CampaignID)
		if err != nil {
			return err
		}
		if !hasAsset(campaign, l.AssetID) {
			return sql.ErrNoRows
		}
	}
	if s.jobs == nil {
		return ErrUnavailable
	}
	row, err := s.repos.Analyses.Ensure(ctx, s.db, l.AssetID, l.CampaignID)
	if err != nil {
		return err
	}
	s.publish(ctx, l, row.ID, row.Status, 0)
	if err := s.jobs.EnqueueAnalyzeAsset(ctx, l.TenantID, l.AssetID, l.CampaignID, ReasonRetry, time.Time{}); err != nil {
		return err
	}
	retryCreatedTotal.Add(1)
	return nil
}

// strandedGrace is how long a pending lens may go without a queued job before
// backfill re-queues it; it covers the gap between creating a lens row and
// queueing its job.
const strandedGrace = 10 * time.Minute

// Backfill queues up to perTenant missing lenses of each active tenant:
// analysable assets without a workspace lens, campaign memberships without a
// campaign lens, and pending lenses whose job was never queued. Each queued
// lens has its row and job, so the next call moves on to the next batch. It
// returns how many lenses it queued.
func (s *Service) Backfill(ctx context.Context, perTenant int) (int, error) {
	if s.jobs == nil {
		return 0, nil
	}
	missing, err := s.repos.Analyses.ListMissingLenses(tenantctx.WithSystem(ctx), perTenant, strandedGrace)
	if err != nil {
		return 0, fmt.Errorf("list missing lenses: %w", err)
	}
	queued := 0
	for _, m := range missing {
		l := Lens{TenantID: m.TenantID, AssetID: m.AssetID, CampaignID: m.CampaignID}
		if s.enqueue(tenantctx.With(ctx, m.TenantID), l, ReasonBackfill) {
			queued++
		}
	}
	backfillEnqueuedTotal.Add(int64(queued))
	return queued, nil
}

// enqueue makes sure the lens row exists, so readers see it pending, and
// queues its job, reporting whether it did. Failures are logged: a trigger
// never fails the write that caused it.
func (s *Service) enqueue(ctx context.Context, l Lens, reason string) bool {
	if s.jobs == nil {
		return false
	}
	if _, err := s.repos.Analyses.Ensure(ctx, s.db, l.AssetID, l.CampaignID); err != nil {
		s.warn(ctx, "create the pending lens", l.AssetID, err)
		return false
	}
	if err := s.jobs.EnqueueAnalyzeAsset(ctx, l.TenantID, l.AssetID, l.CampaignID, reason, time.Time{}); err != nil {
		s.warn(ctx, "queue the analysis", l.AssetID, err)
		return false
	}
	return true
}

func (s *Service) warn(ctx context.Context, what, id string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	slog.WarnContext(ctx, "asset analysis: could not "+what, logging.AttrComponent, logComponent, "id", id, logging.AttrError, err)
}
