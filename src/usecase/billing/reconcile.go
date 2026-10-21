package billing

import (
	"context"
	"errors"
	"log/slog"
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
	if !s.Enabled() {
		return nil
	}
	reconcileRuns.Add(1)

	seen := map[string]bool{}
	var errs []error
	after := ""
	for {
		page, err := s.d.Repo.ListForReconcile(ctx, s.d.Settings.TestMode, after, reconcilePage)
		if err != nil {
			return err
		}
		for _, sub := range page {
			seen[sub.ProviderSubscriptionID] = true
			if err := s.resyncOne(ctx, sub.ProviderSubscriptionID); err != nil {
				errs = append(errs, err)
			}
		}
		if len(page) < reconcilePage {
			break
		}
		after = page[len(page)-1].ID
	}

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
		if err := s.resyncOne(ctx, subID); err != nil {
			errs = append(errs, err)
		}
	}

	if err := s.driveDueChanges(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.sweepUnpaidExpired(ctx); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		slog.WarnContext(ctx, "billing reconcile finished with errors", logging.AttrComponent, "billing", "errors", len(errs), logging.AttrError, errors.Join(errs...))
	}
	return nil
}

func isSubscriptionEvent(name string) bool {
	switch name {
	case "subscription_created", "subscription_updated", "subscription_cancelled", "subscription_resumed",
		"subscription_expired", "subscription_paused", "subscription_unpaused":
		return true
	}
	return false
}

// resyncOne reads one subscription and applies it. A subscription the
// provider no longer knows is left as it is.
func (s *Service) resyncOne(ctx context.Context, providerSubID string) error {
	snap, err := s.d.Provider.GetSubscription(ctx, providerSubID)
	if errors.Is(err, billingdom.ErrNotFound) {
		slog.WarnContext(ctx, "billing reconcile: subscription unknown to the provider", logging.AttrComponent, "billing", "subscription", providerSubID)
		return nil
	}
	if err != nil {
		return err
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

// driveDueChanges runs scheduled-change steps whose time has come, covering
// lost or clock-skewed jobs. RunScheduledChange is idempotent per step.
func (s *Service) driveDueChanges(ctx context.Context) error {
	due, err := s.d.Repo.ListDueScheduledChanges(ctx, s.now(), scheduledChangeLead)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range due {
		if err := s.RunScheduledChange(ctx, c.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
