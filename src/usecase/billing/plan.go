package billing

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// ScheduledChangeView is a pending downgrade, as the entitlements read shows it.
type ScheduledChangeView struct {
	TierID      string    `json:"tier_id"`
	VersionID   string    `json:"version_id"`
	EffectiveAt time.Time `json:"effective_at"`
	Direction   string    `json:"direction"`
}

// PlanState is the billing half of GET /api/me/entitlements: workspace facts,
// no money, readable by every member.
type PlanState struct {
	BillingRequired bool                 `json:"billing_required"`
	TrialEligible   *bool                `json:"trial_eligible"`
	BillingPeriod   *string              `json:"billing_period"`
	RenewsAt        *time.Time           `json:"renews_at"`
	TrialEndsAt     *time.Time           `json:"trial_ends_at"`
	ScheduledChange *ScheduledChangeView `json:"scheduled_change"`
}

// PlanState reads the workspace's billing facts. accountID is the caller's
// account, for the free-month eligibility.
func (s *Service) PlanState(ctx context.Context, tenantID, accountID string) (PlanState, error) {
	var st PlanState
	if !s.Enabled() {
		return st, nil
	}
	gated, err := s.d.Repo.Gated(ctx, tenantID)
	if err != nil {
		return st, err
	}
	st.BillingRequired = gated
	eligible, err := s.TrialEligible(ctx, accountID)
	if err != nil {
		return st, err
	}
	st.TrialEligible = &eligible

	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID)
	if err != nil {
		return st, err
	}
	sub := s.liveSubscription(subs)
	if sub == nil {
		return st, nil
	}
	if sub.Status != models.SubscriptionStatusCancelled {
		st.RenewsAt = sub.RenewsAt
	}
	if sub.Status == models.SubscriptionStatusOnTrial {
		st.TrialEndsAt = sub.TrialEndsAt
	}
	ids := []string{deref(sub.TierVersionPriceID)}
	change, err := s.d.Repo.OpenScheduledChange(ctx, nil, sub.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	if change != nil {
		ids = append(ids, change.FromTierVersionPriceID, change.ToTierVersionPriceID)
	}
	prices, err := s.d.Repo.Prices(ctx, compact(ids...))
	if err != nil {
		return st, err
	}
	periodPrice := deref(sub.TierVersionPriceID)
	if change != nil {
		periodPrice = change.FromTierVersionPriceID
	}
	if p, ok := prices[periodPrice]; ok {
		st.BillingPeriod = new(p.Interval)
	}
	if change != nil {
		to, from := prices[change.ToTierVersionPriceID], prices[change.FromTierVersionPriceID]
		st.ScheduledChange = &ScheduledChangeView{TierID: to.TierID, VersionID: to.TierVersionID, EffectiveAt: change.EffectiveAt, Direction: billingdom.Direction(from, to)}
	}
	return st, nil
}

// PlanRequest is POST /api/workspace/plan.
type PlanRequest struct {
	Buyer
	// TierID is a tier id or a tier version id.
	TierID string
	// PriceID defaults to the version's monthly default-country price.
	PriceID string
}

// PlanResult is either a checkout (no live subscription) or an in-place change.
type PlanResult struct {
	Checkout *Checkout
}

// ChangePlan applies the owner's plan choice. Without a live subscription it
// opens a checkout. With one: an upgrade lands now with proration; a
// downgrade is scheduled for the renewal; choosing the current plan cancels a
// pending downgrade; and any tier change during the free month ends it and
// charges the new tier now. Nothing local changes before the provider agrees.
func (s *Service) ChangePlan(ctx context.Context, req PlanRequest) (PlanResult, error) {
	if !s.Enabled() {
		return PlanResult{}, ErrUnavailable
	}
	if err := s.tenantExists(ctx, req.TenantID); err != nil {
		return PlanResult{}, err
	}
	target, variantID, err := s.resolveTarget(ctx, req.TierID, req.PriceID)
	if err != nil {
		return PlanResult{}, err
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, req.TenantID)
	if err != nil {
		return PlanResult{}, err
	}
	sub := s.liveSubscription(subs)
	if sub == nil {
		co, err := s.startCheckout(ctx, req.Buyer, target, variantID)
		if err != nil {
			return PlanResult{}, err
		}
		return PlanResult{Checkout: &co}, nil
	}
	if !billingdom.Changeable(sub.Status) || sub.TierVersionPriceID == nil {
		return PlanResult{}, ErrNotChangeable
	}
	// While a downgrade is pending the workspace is still on the plan it is
	// leaving, even if the provider already carries the cheaper variant.
	currentID := *sub.TierVersionPriceID
	if open, err := s.d.Repo.OpenScheduledChange(ctx, nil, sub.ID); err == nil {
		currentID = open.FromTierVersionPriceID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return PlanResult{}, err
	}
	prices, err := s.d.Repo.Prices(ctx, []string{currentID})
	if err != nil {
		return PlanResult{}, err
	}
	current, ok := prices[currentID]
	if !ok {
		return PlanResult{}, ErrNotChangeable
	}

	dir := billingdom.Direction(current, target)
	switch {
	case dir == billingdom.DirectionSame:
		return PlanResult{}, s.cancelPendingChange(ctx, sub, current)
	case sub.Status == models.SubscriptionStatusOnTrial:
		return PlanResult{}, s.changeDuringTrial(ctx, req, sub, current, target, variantID)
	case dir == billingdom.DirectionUpgrade:
		return PlanResult{}, s.upgradeNow(ctx, req, sub, current, target, variantID, billingdom.ChangeOpts{InvoiceNow: true}, models.AssignmentReasonUpgrade)
	default:
		return PlanResult{}, s.scheduleDowngrade(ctx, req, sub, current, target)
	}
}

// resolveTarget finds the version and price the owner picked and the variant
// that sells it.
func (s *Service) resolveTarget(ctx context.Context, tierOrVersionID, priceID string) (billingdom.Price, string, error) {
	version, err := s.d.TierVersions.GetByID(ctx, tierOrVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		version, err = s.d.TierVersions.LatestActiveByTier(ctx, tierOrVersionID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return billingdom.Price{}, "", ErrUnknownTier
	}
	if err != nil {
		return billingdom.Price{}, "", err
	}
	if version.Status != models.TierVersionStatusActive || !version.Purchasable {
		return billingdom.Price{}, "", ErrTierNotAvailable
	}
	rows, err := s.d.TierVersions.PricesByVersion(ctx, version.ID)
	if err != nil {
		return billingdom.Price{}, "", err
	}
	if priceID == "" {
		priceID = defaultPrice(rows)
	}
	found := false
	for _, r := range rows {
		found = found || r.ID == priceID
	}
	if !found {
		if priceID == "" {
			return billingdom.Price{}, "", ErrTierNotAvailable
		}
		return billingdom.Price{}, "", ErrPriceMismatch
	}
	variantID, ok := s.d.Settings.Variants.Variant(priceID)
	if !ok {
		return billingdom.Price{}, "", ErrTierNotAvailable
	}
	prices, err := s.d.Repo.Prices(ctx, []string{priceID})
	if err != nil {
		return billingdom.Price{}, "", err
	}
	return prices[priceID], variantID, nil
}

// defaultPrice is the monthly default-country price, else the first monthly,
// else the first price.
func defaultPrice(rows []models.TenantTierVersionPrice) string {
	var monthly, first string
	for _, r := range rows {
		first = cmp.Or(first, r.ID)
		if r.BillingInterval != "month" {
			continue
		}
		if r.CountryCode == nil {
			return r.ID
		}
		monthly = cmp.Or(monthly, r.ID)
	}
	return cmp.Or(monthly, first)
}

// upgradeNow switches the provider first, then mirrors the answer and moves the
// tier in one transaction.
func (s *Service) upgradeNow(ctx context.Context, req PlanRequest, sub *models.BillingSubscription, current, target billingdom.Price, variantID string, opts billingdom.ChangeOpts, reason string) error {
	snap, err := s.d.Provider.ChangePlan(ctx, sub.ProviderSubscriptionID, variantID, opts)
	if err != nil {
		if opts.EndTrialNow && !billingdom.IsRetryable(err) {
			return ErrTrialChangeUnavailable
		}
		return providerErr(err)
	}
	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.d.Repo.GetSubscriptionByProviderID(ctx, tx, sub.Provider, sub.ProviderSubscriptionID, true)
		if err != nil {
			return err
		}
		if err := s.cancelOpenChange(ctx, tx, row.ID, "replaced by a plan change"); err != nil {
			return err
		}
		if snap.UpdatedAt.After(row.ProviderUpdatedAt) {
			s.copySnapshot(row, snap)
		} else {
			row.ProviderVariantID, row.TierVersionPriceID = variantID, new(target.ID)
		}
		row.UpdatedAt = s.now()
		if err := s.d.Repo.UpdateSubscription(ctx, tx, row); err != nil {
			return err
		}
		if current.TierVersionID == target.TierVersionID {
			return nil
		}
		_, err = s.d.Assignments.ReassignTx(ctx, tx, req.TenantID, target.TierID, &target.TierVersionID, reason, s.now())
		return err
	})
	if err != nil {
		return err
	}
	if reason == models.AssignmentReasonDowngrade {
		planChangesDowngrade.Add(1)
	} else {
		planChangesUpgrade.Add(1)
	}
	s.recordPlanChange(ctx, req, "plan_changed", current, target)
	s.publish(ctx, req.TenantID, true)
	return nil
}

// changeDuringTrial: the free month belongs to the marked tier only, so a tier
// change ends it and charges the new tier now. A month/year switch on the same
// tier keeps the trial.
func (s *Service) changeDuringTrial(ctx context.Context, req PlanRequest, sub *models.BillingSubscription, current, target billingdom.Price, variantID string) error {
	reason := models.AssignmentReasonUpgrade
	if billingdom.Direction(current, target) == billingdom.DirectionDowngrade {
		reason = models.AssignmentReasonDowngrade
	}
	opts := billingdom.ChangeOpts{InvoiceNow: true, EndTrialNow: true}
	if current.TierID == target.TierID {
		opts = billingdom.ChangeOpts{NoProration: true}
	}
	return s.upgradeNow(ctx, req, sub, current, target, variantID, opts, reason)
}

// scheduleDowngrade records the downgrade for the renewal date and schedules
// its two steps.
func (s *Service) scheduleDowngrade(ctx context.Context, req PlanRequest, sub *models.BillingSubscription, current, target billingdom.Price) error {
	if sub.RenewsAt == nil {
		return ErrNotChangeable
	}
	effective := *sub.RenewsAt
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		open, err := s.d.Repo.OpenScheduledChange(ctx, tx, sub.ID)
		switch {
		case err == nil && open.ToTierVersionPriceID == target.ID:
			return nil // already scheduled
		case err == nil:
			if open.Status == models.ScheduledChangeProviderApplied {
				// The provider already carries the earlier target; the new
				// change's own step re-points it before the renewal.
				slog.InfoContext(ctx, "billing: replacing a provider-applied downgrade", logging.AttrComponent, "billing", "change_id", open.ID)
			}
			if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, tx, open.ID, []string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied}, models.ScheduledChangeCancelled, "replaced by a new downgrade", s.now()); err != nil {
				return err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		id, err := models.NewID()
		if err != nil {
			return err
		}
		change := &models.BillingScheduledChange{
			ID: id, TenantID: req.TenantID, BillingSubscriptionID: sub.ID,
			FromTierVersionPriceID: current.ID, ToTierVersionPriceID: target.ID,
			EffectiveAt: effective, Status: models.ScheduledChangePending, CreatedBy: req.UserID,
			CreatedAt: s.now(), UpdatedAt: s.now(),
		}
		if err := s.d.Repo.InsertScheduledChange(ctx, tx, change); err != nil {
			return err
		}
		providerStep := effective.Add(-scheduledChangeLead)
		if providerStep.Before(s.now()) {
			providerStep = s.now()
		}
		if err := s.d.Jobs.EnqueueScheduledChangeStepTx(ctx, tx.Tx, id, providerStep); err != nil {
			return err
		}
		return s.d.Jobs.EnqueueScheduledChangeStepTx(ctx, tx.Tx, id, effective)
	})
	if err != nil {
		return err
	}
	planChangesDowngrade.Add(1)
	s.recordPlanChange(ctx, req, "downgrade_scheduled", current, target)
	s.publish(ctx, req.TenantID, true)
	return nil
}

// cancelPendingChange: choosing the current plan cancels a pending downgrade.
// If the provider was already switched, it is switched back first.
func (s *Service) cancelPendingChange(ctx context.Context, sub *models.BillingSubscription, current billingdom.Price) error {
	open, err := s.d.Repo.OpenScheduledChange(ctx, nil, sub.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if open.Status == models.ScheduledChangeProviderApplied {
		variantID, ok := s.d.Settings.Variants.Variant(open.FromTierVersionPriceID)
		if !ok {
			return ErrTierNotAvailable
		}
		snap, err := s.d.Provider.ChangePlan(ctx, sub.ProviderSubscriptionID, variantID, billingdom.ChangeOpts{NoProration: true})
		if err != nil {
			return providerErr(err)
		}
		defer func() {
			if _, err := s.applySnapshot(ctx, snap, nil); err != nil {
				slog.WarnContext(ctx, "billing: mirror after revert failed; the next webhook fixes it", logging.AttrComponent, "billing", logging.AttrError, err)
			}
		}()
	}
	if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, nil, open.ID, []string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied}, models.ScheduledChangeCancelled, "kept the current plan", s.now()); err != nil {
		return err
	}
	s.publish(ctx, sub.TenantID, true)
	return nil
}

// RunScheduledChange advances one scheduled downgrade: at effective_at - 1h
// it switches the provider without proration (so the renewal charges the new
// price); at effective_at it moves the tier. Each step is idempotent, so a
// duplicate job or the reconcile sweep re-driving it is harmless.
func (s *Service) RunScheduledChange(ctx context.Context, changeID string) error {
	if !s.Enabled() {
		return nil
	}
	change, err := s.d.Repo.GetScheduledChange(ctx, nil, changeID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	now := s.now()
	switch {
	case change.Status == models.ScheduledChangePending && !now.Before(change.EffectiveAt.Add(-scheduledChangeLead)):
		return s.pushScheduledChange(ctx, change)
	case change.Status == models.ScheduledChangeProviderApplied && !now.Before(change.EffectiveAt):
		return s.landScheduledChange(ctx, change)
	default:
		return nil // not due yet, or already done
	}
}

func (s *Service) pushScheduledChange(ctx context.Context, change *models.BillingScheduledChange) error {
	sub, err := s.d.Repo.GetSubscription(ctx, change.BillingSubscriptionID)
	if err != nil {
		return err
	}
	variantID, ok := s.d.Settings.Variants.Variant(change.ToTierVersionPriceID)
	if !ok {
		return s.failChange(ctx, change, "target price has no variant")
	}
	snap, err := s.d.Provider.ChangePlan(ctx, sub.ProviderSubscriptionID, variantID, billingdom.ChangeOpts{NoProration: true})
	if err != nil {
		if queues.IsLastAttempt(ctx) || !billingdom.IsRetryable(err) {
			return s.failChange(ctx, change, err.Error())
		}
		return err
	}
	if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, nil, change.ID, []string{models.ScheduledChangePending}, models.ScheduledChangeProviderApplied, "", s.now()); err != nil {
		return err
	}
	// The open change now matches the new variant, so mirroring does not move
	// the tier early.
	if _, err := s.applySnapshot(ctx, snap, nil); err != nil {
		slog.WarnContext(ctx, "billing: mirror after scheduled push failed; the next webhook fixes it", logging.AttrComponent, "billing", logging.AttrError, err)
	}
	return nil
}

func (s *Service) landScheduledChange(ctx context.Context, change *models.BillingScheduledChange) error {
	var moved bool
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		locked, err := s.d.Repo.GetScheduledChange(ctx, tx, change.ID, true)
		if err != nil {
			return err
		}
		if locked.Status != models.ScheduledChangeProviderApplied {
			return nil
		}
		prices, err := s.d.Repo.Prices(ctx, []string{locked.ToTierVersionPriceID})
		if err != nil {
			return err
		}
		to, ok := prices[locked.ToTierVersionPriceID]
		if !ok {
			return fmt.Errorf("billing: scheduled change %s targets a missing price", locked.ID)
		}
		if _, err := s.d.Assignments.ReassignTx(ctx, tx, locked.TenantID, to.TierID, &to.TierVersionID, models.AssignmentReasonDowngrade, locked.EffectiveAt); err != nil {
			return err
		}
		moved = true
		_, err = s.d.Repo.SetScheduledChangeStatus(ctx, tx, locked.ID, []string{models.ScheduledChangeProviderApplied}, models.ScheduledChangeApplied, "", s.now())
		return err
	})
	if err != nil || !moved {
		return err
	}
	s.publish(ctx, change.TenantID, true)
	s.emit(ctx, []notice{{tenantID: change.TenantID, subID: change.BillingSubscriptionID, typ: "plan_changed", level: models.NotificationLevelInfo,
		title: "Your plan changed", body: "Your downgrade took effect with this billing period."}})
	return nil
}

func (s *Service) failChange(ctx context.Context, change *models.BillingScheduledChange, why string) error {
	if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, nil, change.ID, []string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied}, models.ScheduledChangeFailed, why, s.now()); err != nil {
		return err
	}
	slog.ErrorContext(ctx, "billing: scheduled plan change failed", logging.AttrComponent, "billing", "change_id", change.ID, "tenant_id", change.TenantID, "error", why)
	s.emit(ctx, []notice{{tenantID: change.TenantID, subID: change.BillingSubscriptionID, typ: "plan_change_failed", level: models.NotificationLevelError,
		title: "Your plan change didn't go through", body: "We couldn't apply your scheduled downgrade. Your current plan stays in place; please try again."}})
	return nil
}

func (s *Service) recordPlanChange(ctx context.Context, req PlanRequest, typ string, from, to billingdom.Price) {
	if s.d.Activity == nil {
		return
	}
	s.d.Activity.Record(tenantctx.With(ctx, req.TenantID), activity.CategoryBilling, typ,
		activity.WithUser(req.UserID), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"from_price_id": from.ID, "to_price_id": to.ID}))
}
