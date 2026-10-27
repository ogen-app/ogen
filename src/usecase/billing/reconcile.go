package billing

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const (
	reconcilePage          = 100
	webhookRetention       = 90 * 24 * time.Hour
	failedWebhookLookback  = 24 * time.Hour
	unpaidExpiredPageLimit = 200
)

// Reconcile is the periodic safety net behind the webhooks: it re-reads every
// live subscription (and every one a failed webhook touched recently), applies
// it through the webhook path, re-drives due scheduled changes whose jobs were
// lost, and catches unpaid trial expiries the webhook missed. Each part is
// idempotent, so a partial run is simply finished by the next tick.
func (s *Service) Reconcile(ctx context.Context) error {
	plan, err := s.PlanReconcile(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range plan.Subscriptions {
		snap, callErr := s.d.Provider.GetSubscription(ctx, id)
		if err := s.ApplyResync(ctx, id, snap, callErr); err != nil {
			errs = append(errs, err)
		}
	}
	for _, id := range plan.DueChanges {
		if err := s.RunScheduledChange(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		slog.WarnContext(ctx, "billing reconcile finished with errors", logging.AttrComponent, "billing", "errors", len(errs), logging.AttrError, errors.Join(errs...))
	}
	return nil
}

// PlanReconcile does the part of a reconcile pass that needs no provider
// call (catching unpaid trial expiries) and lists the rest. Only a failure to
// list the live subscriptions is returned; the other failures are logged and
// left to the next tick.
func (s *Service) PlanReconcile(ctx context.Context) (billingdom.ReconcilePlan, error) {
	var plan billingdom.ReconcilePlan
	if !s.Enabled() {
		return plan, nil
	}
	reconcileRuns.Add(1)

	seen := map[string]bool{}
	after := ""
	for {
		page, err := s.d.Repo.ListForReconcile(ctx, s.d.Settings.TestMode, after, reconcilePage)
		if err != nil {
			return billingdom.ReconcilePlan{}, err
		}
		for _, sub := range page {
			if !seen[sub.ProviderSubscriptionID] {
				seen[sub.ProviderSubscriptionID] = true
				plan.Subscriptions = append(plan.Subscriptions, sub.ProviderSubscriptionID)
			}
		}
		if len(page) < reconcilePage {
			break
		}
		after = page[len(page)-1].ID
	}

	var errs []error
	failed, err := s.d.Repo.ListFailedWebhookEvents(ctx, s.now().Add(-failedWebhookLookback), false)
	if err != nil {
		errs = append(errs, err)
	}
	for _, ev := range failed {
		subID := ev.ProviderObjectID
		if ev.EventName == "" || seen[subID] || subID == "" || !isSubscriptionEvent(ev.EventName) {
			continue
		}
		seen[subID] = true
		plan.Subscriptions = append(plan.Subscriptions, subID)
	}

	due, err := s.d.Repo.ListDueScheduledChanges(ctx, s.now(), scheduledChangeLead)
	if err != nil {
		errs = append(errs, err)
	}
	for _, c := range due {
		plan.DueChanges = append(plan.DueChanges, c.ID)
	}
	if err := s.sweepUnpaidExpired(ctx); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		slog.WarnContext(ctx, "billing reconcile could not finish every check", logging.AttrComponent, "billing", "errors", len(errs), logging.AttrError, errors.Join(errs...))
	}
	return plan, nil
}

// subscriptionEvents are the webhooks that carry a subscription snapshot; their
// object id is the subscription id.
var subscriptionEvents = []string{
	"subscription_created", "subscription_updated", "subscription_cancelled", "subscription_resumed",
	"subscription_expired", "subscription_paused", "subscription_unpaused",
}

func isSubscriptionEvent(name string) bool {
	return slices.Contains(subscriptionEvents, name)
}

// ApplyResync applies one re-read subscription: callErr is the read's
// error. A subscription the provider no longer knows is left as it is.
func (s *Service) ApplyResync(ctx context.Context, providerSubID string, snap billingdom.Subscription, callErr error) error {
	if errors.Is(callErr, billingdom.ErrNotFound) {
		slog.WarnContext(ctx, "billing reconcile: subscription unknown to the provider", logging.AttrComponent, "billing", "subscription", providerSubID)
		return nil
	}
	if callErr != nil {
		return callErr
	}
	if snap.TestMode != s.d.Settings.TestMode {
		return nil
	}
	res, err := s.applySnapshot(ctx, snap, nil)
	if err != nil {
		return err
	}
	if res.changed {
		reconcileCorrections.Add(1)
	}
	return nil
}

// ResyncTenant re-reads every non-invalidated subscription of one workspace.
// It returns how many mirrors changed.
func (s *Service) ResyncTenant(ctx context.Context, tenantID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrUnavailable
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID)
	if err != nil {
		return 0, err
	}
	corrected := 0
	for _, sub := range subs {
		if sub.InvalidatedAt != nil || sub.TestMode != s.d.Settings.TestMode {
			continue
		}
		snap, err := s.d.Provider.GetSubscription(ctx, sub.ProviderSubscriptionID)
		if errors.Is(err, billingdom.ErrNotFound) {
			continue
		}
		if err != nil {
			return corrected, providerErr(err)
		}
		res, err := s.applySnapshot(ctx, snap, nil)
		if err != nil {
			return corrected, err
		}
		if res.changed {
			corrected++
		}
	}
	return corrected, nil
}

// sweepUnpaidExpired catches trials that expired without a payment whose
// teardown was not enqueued by the webhook path.
func (s *Service) sweepUnpaidExpired(ctx context.Context) error {
	if !s.d.Settings.TrialTeardownLive {
		return nil
	}
	subs, err := s.d.Repo.ListUnpaidExpired(ctx, s.d.Settings.TestMode, unpaidExpiredPageLimit)
	if err != nil {
		return err
	}
	var errs []error
	for _, sub := range subs {
		err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			locked, err := s.d.Repo.GetSubscriptionByProviderID(ctx, tx, sub.Provider, sub.ProviderSubscriptionID, true)
			if err != nil {
				return err
			}
			if locked.Status != models.SubscriptionStatusExpired || locked.FirstPaidAt != nil {
				return nil
			}
			return s.teardownUnpaid(ctx, tx, locked)
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SweepWebhookEvents prunes webhook deliveries older than 90 days.
func (s *Service) SweepWebhookEvents(ctx context.Context) error {
	n, err := s.d.Repo.DeleteWebhookEventsBefore(ctx, s.now().Add(-webhookRetention))
	if err != nil {
		return err
	}
	if n > 0 {
		slog.InfoContext(ctx, "billing webhook log pruned", logging.AttrComponent, "billing", "deleted", n)
	}
	return nil
}

// providerErr wraps a provider failure as ErrProvider unless it is a
// configuration gap.
func providerErr(err error) error {
	if errors.Is(err, billingdom.ErrNotConfigured) {
		return ErrUnavailable
	}
	return errors.Join(ErrProvider, err)
}
