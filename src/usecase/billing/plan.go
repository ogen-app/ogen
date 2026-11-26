package billing

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
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
	BillingRequired bool `json:"billing_required"`
	// Complimentary marks a tester's workspace: never billed, and choosing a
	// plan switches it at once with no checkout. Reported whether or not
	// billing is on.
	Complimentary   bool                 `json:"complimentary"`
	TrialEligible   *bool                `json:"trial_eligible" extensions:"x-nullable"`
	BillingPeriod   *string              `json:"billing_period" extensions:"x-nullable"`
	RenewsAt        *time.Time           `json:"renews_at" extensions:"x-nullable"`
	TrialEndsAt     *time.Time           `json:"trial_ends_at" extensions:"x-nullable"`
	ScheduledChange *ScheduledChangeView `json:"scheduled_change" extensions:"x-nullable"`
}

// PlanState reads the workspace's billing facts. accountID is the caller's
// account, for the free-month eligibility.
func (s *Service) PlanState(ctx context.Context, tenantID, accountID string) (PlanState, error) {
	var st PlanState
	comp, err := s.complimentaryTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, ErrTenantNotFound) {
		return st, err
	}
	st.Complimentary = comp
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

// ChangePlan applies the owner's plan choice. A complimentary workspace with
// no live subscription switches at once, with no checkout and no provider
// call. Otherwise, without a live subscription it opens a checkout. With
// one: an upgrade lands now with proration; a downgrade is held by the
// provider for the renewal; choosing the current plan cancels a pending
// downgrade; and any tier change during the free month ends it and charges
// the new tier now. Nothing local changes before the provider agrees.
func (s *Service) ChangePlan(ctx context.Context, req PlanRequest) (PlanResult, error) {
	// A complimentary workspace switches for free, billing on or off.
	if comp, err := s.compChangeApplies(ctx, req.TenantID); err != nil {
		return PlanResult{}, err
	} else if comp {
		return PlanResult{}, s.compChangePlan(ctx, req)
	}
	if !s.Enabled() {
		return PlanResult{}, ErrUnavailable
	}
	if err := s.tenantExists(ctx, req.TenantID); err != nil {
		return PlanResult{}, err
	}
	target, productID, err := s.resolveTarget(ctx, req.TierID, req.PriceID)
	if err != nil {
		return PlanResult{}, err
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, req.TenantID)
	if err != nil {
		return PlanResult{}, err
	}
	sub := s.liveSubscription(subs)
	if sub == nil {
		co, err := s.startCheckout(ctx, req.Buyer, target, productID)
		if err != nil {
			return PlanResult{}, err
		}
		return PlanResult{Checkout: &co}, nil
	}
	if !billingdom.Changeable(sub.Status) || sub.TierVersionPriceID == nil {
		return PlanResult{}, ErrNotChangeable
	}
	// While a downgrade is pending the workspace is still on the plan it is
	// leaving.
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
		return PlanResult{}, s.cancelPendingChange(ctx, sub)
	case sub.Status == models.SubscriptionStatusOnTrial:
		return PlanResult{}, s.changeDuringTrial(ctx, req, sub, current, target, productID)
	case dir == billingdom.DirectionUpgrade:
		return PlanResult{}, s.upgradeNow(ctx, req, sub, current, target, productID, billingdom.ChangeOpts{Timing: billingdom.ChargeNow}, models.AssignmentReasonUpgrade)
	default:
		return PlanResult{}, s.scheduleDowngrade(ctx, req, sub, current, target, productID)
	}
}

// resolveTarget finds the version and price the owner picked and the product
// that sells it. A price whose product is unmapped or archived cannot be
// bought.
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
	products, err := s.productMap(ctx)
	if err != nil {
		return billingdom.Price{}, "", err
	}
	productID, ok := products.Product(priceID)
	if !ok {
		return billingdom.Price{}, "", ErrTierNotAvailable
	}
	if p, err := s.product(ctx, productID); err == nil && p.Archived {
		return billingdom.Price{}, "", ErrTierNotAvailable
	}
	prices, err := s.d.Repo.Prices(ctx, []string{priceID})
	if err != nil {
		return billingdom.Price{}, "", err
	}
	return prices[priceID], productID, nil
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

// lockedRetryDelay spaces the one retry of a change the provider refused
// because another change of the subscription was in flight.
var lockedRetryDelay = time.Second

// changeAtProvider makes a plan change at the provider and maps its refusals:
// a declined or unauthenticated charge is ErrPaymentFailed, a subscription
// the provider already cancelled is ErrNotChangeable (and the mirror is
// re-read), a locked subscription is retried once.
func (s *Service) changeAtProvider(ctx context.Context, sub *models.BillingSubscription, productID string, opts billingdom.ChangeOpts) (billingdom.Subscription, error) {
	snap, err := s.d.Provider.ChangePlan(ctx, sub.ProviderSubscriptionID, productID, opts)
	if locked(err) {
		select {
		case <-ctx.Done():
			return billingdom.Subscription{}, ctx.Err()
		case <-time.After(lockedRetryDelay):
		}
		snap, err = s.d.Provider.ChangePlan(ctx, sub.ProviderSubscriptionID, productID, opts)
	}
	switch {
	case err == nil:
		return snap, nil
	case errors.Is(err, billingdom.ErrPaymentFailed):
		return billingdom.Subscription{}, ErrPaymentFailed
	case errors.Is(err, billingdom.ErrAlreadyCanceled):
		s.refresh(ctx, sub.ProviderSubscriptionID)
		return billingdom.Subscription{}, ErrNotChangeable
	case opts.EndTrialNow && !billingdom.IsRetryable(err):
		return billingdom.Subscription{}, ErrTrialChangeUnavailable
	}
	countProviderError(err)
	return billingdom.Subscription{}, providerErr(err)
}

// locked reports whether the provider refused a change because another one
// was in flight.
func locked(err error) bool {
	l, ok := errors.AsType[interface {
		error
		Locked() bool
	}](err)
	return ok && l.Locked()
}

// refresh re-reads a subscription and mirrors it; a failure is only logged,
// the reconcile sweep catches up.
func (s *Service) refresh(ctx context.Context, providerSubID string) {
	snap, err := s.d.Provider.GetSubscription(ctx, providerSubID)
	if err != nil {
		slog.WarnContext(ctx, "billing: re-read after a refused change failed", logging.AttrComponent, "billing", "subscription", providerSubID, logging.AttrError, err)
		return
	}
	s.mirrorAfter(ctx, snap, "refused change")
}

// upgradeNow switches the provider first, then mirrors the answer and moves the
// tier in one transaction. A downgrade still pending is cancelled at the
// provider before, so two changes are never held at once.
func (s *Service) upgradeNow(ctx context.Context, req PlanRequest, sub *models.BillingSubscription, current, target billingdom.Price, productID string, opts billingdom.ChangeOpts, reason string) error {
	open, err := s.d.Repo.OpenScheduledChange(ctx, nil, sub.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := s.dropProviderChange(ctx, sub, open); err != nil {
		return err
	}
	// The provider no longer holds the downgrade, so it is cancelled here
	// too, whether or not the charge below goes through: a declined upgrade
	// must not leave a downgrade on screen that the provider dropped.
	if open != nil {
		if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, nil, open.ID, []string{open.Status}, models.ScheduledChangeCancelled, "replaced by a plan change", s.now()); err != nil {
			return err
		}
		s.publish(ctx, sub.TenantID, true)
	}
	snap, err := s.changeAtProvider(ctx, sub, productID, opts)
	if err != nil {
		return err
	}
	products, err := s.productMap(ctx)
	if err != nil {
		return err
	}
	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.d.Repo.GetSubscriptionByProviderID(ctx, tx, sub.Provider, sub.ProviderSubscriptionID, true)
		if err != nil {
			return err
		}
		if err := s.cancelOpenChange(ctx, tx, row.ID, "replaced by a plan change"); err != nil {
			return err
		}
		if snap.UpdatedAt.After(row.ProviderUpdatedAt) && snap.Status != "" {
			s.copySnapshot(row, snap, products)
			// The product was resolved to target before the switch; a
			// mapping changed since must not detach the price the tier moves to.
			if snap.ProductID == productID {
				row.TierVersionPriceID = new(target.ID)
			}
		} else {
			row.ProviderProductID, row.ProviderVariantID, row.TierVersionPriceID = productID, productID, new(target.ID)
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
func (s *Service) changeDuringTrial(ctx context.Context, req PlanRequest, sub *models.BillingSubscription, current, target billingdom.Price, productID string) error {
	reason := models.AssignmentReasonUpgrade
	if billingdom.Direction(current, target) == billingdom.DirectionDowngrade {
		reason = models.AssignmentReasonDowngrade
	}
	opts := billingdom.ChangeOpts{Timing: billingdom.ChargeNow, EndTrialNow: true}
	if current.TierID == target.TierID {
		opts = billingdom.ChangeOpts{Timing: billingdom.NoCharge}
	}
	return s.upgradeNow(ctx, req, sub, current, target, productID, opts, reason)
}

// scheduleDowngrade has the provider hold the downgrade for the renewal, then
// records it. The provider applies it at renewal and the renewal's snapshot
// moves the tier; no job runs. Picking another lower plan replaces it.
func (s *Service) scheduleDowngrade(ctx context.Context, req PlanRequest, sub *models.BillingSubscription, current, target billingdom.Price, productID string) error {
	if sub.RenewsAt == nil {
		return ErrNotChangeable
	}
	open, err := s.d.Repo.OpenScheduledChange(ctx, nil, sub.ID)
	switch {
	case err == nil && open.ToTierVersionPriceID == target.ID:
		return nil // already scheduled
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	snap, err := s.changeAtProvider(ctx, sub, productID, billingdom.ChangeOpts{Timing: billingdom.NextPeriod})
	if err != nil {
		return err
	}
	effective := *sub.RenewsAt
	switch {
	case snap.PendingEffectiveAt != nil:
		effective = *snap.PendingEffectiveAt
	case snap.RenewsAt != nil:
		effective = *snap.RenewsAt
	}
	products, err := s.productMap(ctx)
	if err != nil {
		return err
	}
	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.d.Repo.GetSubscriptionByProviderID(ctx, tx, sub.Provider, sub.ProviderSubscriptionID, true)
		if err != nil {
			return err
		}
		if err := s.cancelOpenChange(ctx, tx, row.ID, "replaced by a new downgrade"); err != nil {
			return err
		}
		id, err := models.NewID()
		if err != nil {
			return err
		}
		if err := s.d.Repo.InsertScheduledChange(ctx, tx, &models.BillingScheduledChange{
			ID: id, TenantID: req.TenantID, BillingSubscriptionID: row.ID,
			FromTierVersionPriceID: current.ID, ToTierVersionPriceID: target.ID,
			EffectiveAt: effective, Status: models.ScheduledChangeProviderApplied, CreatedBy: req.UserID,
			CreatedAt: s.now(), UpdatedAt: s.now(),
		}); err != nil {
			return err
		}
		if !snap.UpdatedAt.After(row.ProviderUpdatedAt) || snap.Status == "" {
			return nil
		}
		s.copySnapshot(row, snap, products)
		row.UpdatedAt = s.now()
		return s.d.Repo.UpdateSubscription(ctx, tx, row)
	})
	if err != nil {
		return err
	}
	planChangesDowngrade.Add(1)
	s.recordPlanChange(ctx, req, "downgrade_scheduled", current, target)
	s.publish(ctx, req.TenantID, true)
	return nil
}

// cancelPendingChange: choosing the current plan cancels a pending downgrade,
// at the provider first. The provider's is dropped even without a local row
// (its write may have failed after the provider took the change).
func (s *Service) cancelPendingChange(ctx context.Context, sub *models.BillingSubscription) error {
	open, err := s.d.Repo.OpenScheduledChange(ctx, nil, sub.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.dropProviderChange(ctx, sub, nil)
	}
	if err != nil {
		return err
	}
	if err := s.dropProviderChange(ctx, sub, open); err != nil {
		return err
	}
	if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, nil, open.ID, []string{open.Status}, models.ScheduledChangeCancelled, "kept the current plan", s.now()); err != nil {
		return err
	}
	s.publish(ctx, sub.TenantID, true)
	return nil
}

// dropProviderChange clears the change the provider holds for the
// subscription's next period, when the mirror shows one or a downgrade the
// provider holds is open (the mirror may not have caught up), and mirrors the
// answer.
func (s *Service) dropProviderChange(ctx context.Context, sub *models.BillingSubscription, open *models.BillingScheduledChange) error {
	held := open != nil && open.Status == models.ScheduledChangeProviderApplied
	if sub.PendingProductID == "" && !held {
		return nil
	}
	snap, err := s.d.Provider.CancelScheduledChange(ctx, sub.ProviderSubscriptionID, sub.ProviderProductID)
	if err != nil {
		countProviderError(err)
		return providerErr(err)
	}
	s.mirrorAfter(ctx, snap, "cancelled downgrade")
	sub.PendingProductID, sub.PendingEffectiveAt = snap.PendingProductID, snap.PendingEffectiveAt
	return nil
}

// mirrorAfter applies a provider snapshot taken after our own change; a
// failure is only logged because the webhook for that change re-applies it.
func (s *Service) mirrorAfter(ctx context.Context, snap billingdom.Subscription, step string) {
	if _, err := s.applySnapshot(ctx, snap); err != nil {
		slog.WarnContext(ctx, "billing: mirror after "+step+" failed; the next webhook fixes it", logging.AttrComponent, "billing", logging.AttrError, err)
	}
}

// scheduledChangeGrace is how long after its effective time a downgrade the
// provider holds may take to land before reconcile calls it failed.
const scheduledChangeGrace = time.Hour

// failOverdueChanges fails the downgrades that should have landed at renewal
// but did not: past effective_at by the grace, the mirror is still on another
// product and the provider holds nothing. The tier stays where it is; the
// operator decides (Harbor attention: scheduled_change_failed).
func (s *Service) failOverdueChanges(ctx context.Context) error {
	changes, err := s.d.Repo.ListOverdueScheduledChanges(ctx, s.now().Add(-scheduledChangeGrace))
	if err != nil {
		return err
	}
	products, err := s.productMap(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range changes {
		c := &changes[i]
		sub, err := s.d.Repo.GetSubscription(ctx, c.BillingSubscriptionID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !s.ours(sub) || sub.PendingProductID != "" {
			continue
		}
		if target, ok := products.Product(c.ToTierVersionPriceID); ok && target == sub.ProviderProductID {
			continue // landed; its snapshot marks it applied
		}
		if err := s.failChange(ctx, c, "the provider did not apply the downgrade at renewal"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) failChange(ctx context.Context, change *models.BillingScheduledChange, why string) error {
	if _, err := s.d.Repo.SetScheduledChangeStatus(ctx, nil, change.ID, []string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied}, models.ScheduledChangeFailed, why, s.now()); err != nil {
		return err
	}
	slog.ErrorContext(ctx, "billing: scheduled plan change failed", logging.AttrComponent, "billing", "change_id", change.ID, "tenant_id", change.TenantID, "error", why)
	s.emit(ctx, []notice{{tenantID: change.TenantID, subID: change.BillingSubscriptionID, typ: "plan_change_failed", episode: change.ID, level: models.NotificationLevelError,
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

// countProviderError counts a provider failure by HTTP status, when it has one.
func countProviderError(err error) {
	key := "transport"
	if st, ok := errors.AsType[interface {
		error
		HTTPStatus() int
	}](err); ok {
		key = strconv.Itoa(st.HTTPStatus())
	}
	providerErrors.Add(key, 1)
}
