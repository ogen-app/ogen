// Package create makes campaigns: the one path the REST endpoint and the
// assistant share, so both gate, meter and announce a new campaign alike.
package create

import (
	"context"
	"errors"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// ErrInvalidType is a campaign type the workspace doesn't have.
var ErrInvalidType = errors.New("invalid campaign_type_id")

// baselineTypeSlug is the one system campaign type every tier can use; the
// other system types are gated by all_campaign_types.
const baselineTypeSlug = "evergreen"

// AnalysisTrigger queues the campaign lenses of assets just attached;
// *assetanalysis.Service implements it.
type AnalysisTrigger interface {
	ScheduleAttached(ctx context.Context, tenantID, campaignID string, assetIDs []string)
}

// Service creates campaigns. Limiter, Analysis and Activity may be nil.
type Service struct {
	Campaigns repository.CampaignRepository
	Types     repository.CampaignTypeRepository
	Limiter   *entitlements.Limiter
	Analysis  AnalysisTrigger
	Activity  *activity.Recorder
}

// Create stores c once the active_campaigns quota and its campaign type's
// entitlement gate allow it, then fires the quota's near-limit crossing,
// queues the analyses of its assets and records campaign_created from
// source. c arrives complete but for its type, which Create loads.
func (s *Service) Create(ctx context.Context, c *models.Campaign, source string) error {
	tenantID, scoped := tenantctx.From(ctx)
	var quota entitlements.Decision
	if scoped {
		var err error
		if quota, err = s.Limiter.Require(ctx, tenantID, "active_campaigns"); err != nil {
			return err
		}
	}
	ct, err := s.Types.GetByID(ctx, c.CampaignTypeID)
	if err != nil {
		return ErrInvalidType
	}
	if err := s.GateType(ctx, ct); err != nil {
		return err
	}
	if err := s.Campaigns.Create(ctx, c); err != nil {
		return err
	}
	if scoped {
		s.Limiter.DispatchCrossing(ctx, tenantID, quota)
		if s.Analysis != nil && len(c.AssetIDs) > 0 {
			s.Analysis.ScheduleAttached(ctx, tenantID, c.ID, c.AssetIDs)
		}
	}
	s.Activity.Record(ctx, activity.CategoryCampaign, "campaign_created",
		activity.WithSource(source),
		activity.WithEntity("campaign", c.ID),
		activity.WithPayload(map[string]any{"status": string(c.Status), "campaign_type_id": c.CampaignTypeID}),
	)
	return nil
}

// GateType enforces the campaign-type entitlement gates: a custom
// (non-system) type requires custom_campaign_types, a non-baseline system
// type all_campaign_types. Evergreen is always allowed. The error is the
// limiter's *FeatureNotAvailableError.
func (s *Service) GateType(ctx context.Context, ct *models.CampaignType) error {
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return nil
	}
	switch {
	case !ct.IsSystem:
		return s.Limiter.RequireGate(ctx, tenantID, "custom_campaign_types")
	case ct.Name != baselineTypeSlug:
		return s.Limiter.RequireGate(ctx, tenantID, "all_campaign_types")
	}
	return nil
}
