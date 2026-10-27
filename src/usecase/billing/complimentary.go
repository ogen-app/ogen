package billing

import (
	"context"
	"database/sql"
	"errors"
	"expvar"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// ErrComplimentary: the operator tried to gate a complimentary workspace with
// SetBillingRequired; ending complimentary access is SetComplimentary(false).
var ErrComplimentary = errors.New("workspace is complimentary")

var compPlanChanges = expvar.NewInt("billing_comp_plan_changes")

// complimentaryTenant loads the workspace and reports whether it is a
// tester's complimentary one.
func (s *Service) complimentaryTenant(ctx context.Context, tenantID string) (bool, error) {
	t, err := s.d.Tenants.GetByID(ctx, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrTenantNotFound
	}
	if err != nil {
		return false, err
	}
	if t.DeletedAt != nil {
		return false, ErrTenantNotFound
	}
	return t.Complimentary, nil
}

// compChangeApplies reports whether a plan choice takes the complimentary
// path: the workspace is complimentary and follows no live subscription (a
// subscription, however it came about, keeps the normal rules).
func (s *Service) compChangeApplies(ctx context.Context, tenantID string) (bool, error) {
	comp, err := s.complimentaryTenant(ctx, tenantID)
	if err != nil || !comp {
		return false, err
	}
	if !s.Enabled() {
		return true, nil
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID)
	if err != nil {
		return false, err
	}
	return s.liveSubscription(subs) == nil, nil
}

// compChangePlan moves a complimentary workspace to the chosen tier with no
// provider call and no subscription: testers try any public plan for free. The
// target must be a published, purchasable version (the plan picker's set);
// hidden tiers stay operator-only. Choosing the current version is a no-op.
func (s *Service) compChangePlan(ctx context.Context, req PlanRequest) error {
	version, err := s.d.TierVersions.GetByID(ctx, req.TierID)
	if errors.Is(err, sql.ErrNoRows) {
		version, err = s.d.TierVersions.LatestActiveByTier(ctx, req.TierID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownTier
	}
	if err != nil {
		return err
	}
	if version.Status != models.TierVersionStatusActive || !version.Purchasable {
		return ErrTierNotAvailable
	}
	now := s.now()
	current, err := s.d.Assignments.CoveringAt(ctx, req.TenantID, now)
	switch {
	case err == nil && current.TierVersionID == version.ID:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	ok, err := s.d.Assignments.ReassignActive(ctx, req.TenantID, version.ID, models.AssignmentReasonComp, now)
	if errors.Is(err, repository.ErrVersionNotAssignable) {
		return ErrTierNotAvailable
	}
	if err != nil {
		return err
	}
	if !ok {
		return ErrTenantNotFound
	}
	compPlanChanges.Add(1)
	if s.d.Activity != nil {
		from := ""
		if current != nil {
			from = current.TierVersionID
		}
		s.d.Activity.Record(tenantctx.With(ctx, req.TenantID), activity.CategoryBilling, "plan_changed",
			activity.WithUser(req.UserID), activity.WithSource(activity.SourceAPI),
			activity.WithPayload(map[string]any{"complimentary": true, "from_version_id": from, "to_version_id": version.ID}))
	}
	s.publish(ctx, req.TenantID, true)
	return nil
}

// SetComplimentary turns a workspace into a tester's complimentary one (never
// billed, free tier switches) or ends that: the workspace then goes behind the
// billing gate until it checks out, keeping its tier and data. The owner still
// gets the free month if their account never had one.
func (s *Service) SetComplimentary(ctx context.Context, tenantID string, complimentary bool, reason string) error {
	if reason == "" {
		return ErrReasonRequired
	}
	ok, err := s.d.Repo.SetComplimentary(ctx, tenantID, complimentary, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrTenantNotFound
	}
	if s.d.Activity != nil {
		s.d.Activity.Record(tenantctx.With(ctx, tenantID), activity.CategoryBilling, "complimentary_changed",
			activity.WithSource(activity.SourceSystem),
			activity.WithPayload(map[string]any{"complimentary": complimentary, "reason": reason}))
	}
	s.publish(ctx, tenantID, true)
	return nil
}
