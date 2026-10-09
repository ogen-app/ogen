package assetanalysis

import (
	"context"
	"database/sql"
	"errors"
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
)

// DefaultRebriefDebounce is how long a brief edit waits before its campaign's
// lenses re-run, so a burst of saves costs one run.
const DefaultRebriefDebounce = 10 * time.Minute

// Jobs enqueues the analysis jobs. *queues.Enqueuer implements it.
type Jobs interface {
	EnqueueAnalyzeAsset(ctx context.Context, tenantID, assetID string, campaignID *string, reason string) error
	EnqueueReanalyzeCampaign(ctx context.Context, tenantID, campaignID string, at time.Time) error
}

// ErrNotRetryable means the lens is neither failed nor skipped.
var ErrNotRetryable = errors.New("analysis is not failed or skipped")

// ErrNotAnalyzable means the asset type is never analysed.
var ErrNotAnalyzable = errors.New("asset type is not analysed")

// SetJobs binds the job queue and the brief-edit debounce.
func (s *Service) SetJobs(jobs Jobs, rebriefDebounce time.Duration) {
	s.jobs = jobs
	s.rebriefDebounce = rebriefDebounce
	if s.rebriefDebounce <= 0 {
		s.rebriefDebounce = DefaultRebriefDebounce
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

// Retry re-queues a failed or skipped lens.
func (s *Service) Retry(ctx context.Context, tenantID, assetID string, campaignID *string) error {
	ctx = tenantctx.With(ctx, tenantID)
	row, err := s.repos.Analyses.GetLens(ctx, assetID, campaignID)
	if err != nil {
		return err
	}
	if !row.Status.Retryable() {
		return ErrNotRetryable
	}
	if err := s.repos.Analyses.SetStatus(ctx, s.db, row.ID, models.AssetAnalysisStatusPending, "", ""); err != nil {
		return err
	}
	l := Lens{TenantID: tenantID, AssetID: assetID, CampaignID: campaignID}
	s.publish(ctx, l, row.ID, models.AssetAnalysisStatusPending, 0)
	if s.jobs == nil {
		return ErrUnavailable
	}
	return s.jobs.EnqueueAnalyzeAsset(ctx, tenantID, assetID, campaignID, ReasonRetry)
}

// enqueue makes sure the lens row exists, so readers see it pending, and
// queues its job. Failures are logged: a trigger never fails the write that
// caused it.
func (s *Service) enqueue(ctx context.Context, l Lens, reason string) {
	if s.jobs == nil {
		return
	}
	if _, err := s.repos.Analyses.Ensure(ctx, s.db, l.AssetID, l.CampaignID); err != nil {
		s.warn(ctx, "create the pending lens", l.AssetID, err)
		return
	}
	if err := s.jobs.EnqueueAnalyzeAsset(ctx, l.TenantID, l.AssetID, l.CampaignID, reason); err != nil {
		s.warn(ctx, "queue the analysis", l.AssetID, err)
	}
}

func (s *Service) warn(ctx context.Context, what, id string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	slog.WarnContext(ctx, "asset analysis: could not "+what, logging.AttrComponent, logComponent, "id", id, logging.AttrError, err)
}
