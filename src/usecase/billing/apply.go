package billing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// errUnresolvedTenant marks a webhook whose workspace cannot be found. It is
// recorded for Harbor and acknowledged: a retry would not find it either.
var errUnresolvedTenant = errors.New("subscription tenant could not be resolved")

// HandleWebhook verifies, logs and applies one provider webhook. It returns
// ErrUnavailable without a configured secret, ErrInvalidSignature on a bad
// signature, and any other error when applying failed (the caller answers 5xx
// so the provider retries).
func (s *Service) HandleWebhook(ctx context.Context, signature string, body []byte) error {
	if !s.Enabled() {
		return ErrUnavailable
	}
	if err := s.d.Provider.VerifyWebhook(ctx, signature, body); err != nil {
		switch {
		case errors.Is(err, billingdom.ErrNotConfigured):
			return ErrUnavailable
		case errors.Is(err, billingdom.ErrInvalidSignature):
			return ErrInvalidSignature
		default:
			return err
		}
	}
	webhooksReceived.Add(1)
	ev, err := s.d.Provider.ParseWebhook(body)
	if err != nil {
		return fmt.Errorf("billing webhook: %w", err)
	}
	// Payments are kept in both modes, apart by test_mode, and before the
	// event is logged: a failure here makes the provider redeliver, and a
	// redelivery of an applied event stops at the log below.
	if err := s.recordWebhookPayments(ctx, ev); err != nil {
		return err
	}

	sum := sha256.Sum256(body)
	id, err := models.NewID()
	if err != nil {
		return err
	}
	row := &models.BillingWebhookEvent{
		ID: id, Provider: s.provider(), EventName: ev.Name, BodySHA256: hex.EncodeToString(sum[:]),
		ProviderObjectID: ev.ObjectID, TestMode: ev.TestMode, ReceivedAt: s.now(), Payload: json.RawMessage(body),
	}
	// The tenant is stamped only once applying resolved it: custom data is
	// unverified input and may name a workspace that does not exist.
	existing, inserted, err := s.d.Repo.InsertWebhookEvent(ctx, row)
	if err != nil {
		return err
	}
	if !inserted {
		if existing.ProcessedAt != nil && existing.Outcome != models.WebhookOutcomeFailed {
			return nil // a redelivery of something already applied
		}
		row = existing
	}

	outcome, tenantID, applyErr := s.applyEvent(ctx, ev)
	errMsg := ""
	if applyErr != nil {
		outcome, errMsg = models.WebhookOutcomeFailed, applyErr.Error()
	}
	var tid *string
	if tenantID != "" {
		tid = &tenantID
	}
	if err := s.d.Repo.FinishWebhookEvent(ctx, row.ID, outcome, errMsg, tid, s.now()); err != nil {
		slog.WarnContext(ctx, "billing webhook: outcome write failed", logging.AttrComponent, "billing", logging.AttrError, err)
	}
	countWebhookOutcome(outcome)
	if applyErr != nil && !errors.Is(applyErr, errUnresolvedTenant) {
		return applyErr
	}
	return nil
}

func countWebhookOutcome(outcome string) {
	switch outcome {
	case models.WebhookOutcomeApplied:
		webhooksApplied.Add(1)
	case models.WebhookOutcomeStale:
		webhooksStale.Add(1)
	case models.WebhookOutcomeIgnoredMode:
		webhooksIgnoredMode.Add(1)
	case models.WebhookOutcomeFailed:
		webhooksFailed.Add(1)
	}
}

// applyEvent routes one parsed event. It returns the outcome and the tenant.
func (s *Service) applyEvent(ctx context.Context, ev billingdom.Event) (string, string, error) {
	if ev.TestMode != s.d.Settings.TestMode {
		slog.WarnContext(ctx, "billing webhook from the other mode ignored", logging.AttrComponent, "billing", "event", ev.Name, "test_mode", ev.TestMode)
		return models.WebhookOutcomeIgnoredMode, "", nil
	}
	switch ev.Kind {
	case billingdom.EventSubscription:
		res, err := s.applySnapshot(ctx, *ev.Subscription, ev.CustomData)
		return res.outcome, res.tenantID, err
	case billingdom.EventPayment:
		return s.applyPayment(ctx, ev)
	default:
		return models.WebhookOutcomeIgnoredUnknown, "", nil
	}
}

// applyPayment records the first successful charge, which separates a paying
// workspace from a trial that never converted.
func (s *Service) applyPayment(ctx context.Context, ev billingdom.Event) (string, string, error) {
	if ev.Name != "subscription_payment_success" || ev.PaymentAmountMinor <= 0 {
		sub, err := s.d.Repo.GetSubscriptionByProviderID(ctx, nil, s.provider(), ev.PaymentSubscriptionID, false)
		if err == nil {
			return models.WebhookOutcomeApplied, sub.TenantID, nil
		}
		return models.WebhookOutcomeApplied, "", nil
	}
	var tenantID string
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		sub, err := s.d.Repo.GetSubscriptionByProviderID(ctx, tx, s.provider(), ev.PaymentSubscriptionID, true)
		if errors.Is(err, sql.ErrNoRows) {
			return s.unknownSubscriptionPayment(ctx, ev.PaymentSubscriptionID)
		}
		if err != nil {
			return err
		}
		tenantID = sub.TenantID
		if sub.FirstPaidAt != nil {
			return nil
		}
		sub.FirstPaidAt = new(s.now())
		sub.UpdatedAt = s.now()
		return s.d.Repo.UpdateSubscription(ctx, tx, sub)
	})
	if err != nil {
		return "", tenantID, err
	}
	return models.WebhookOutcomeApplied, tenantID, nil
}

// unknownSubscriptionPayment explains a payment for a subscription that is
// not mirrored. When a snapshot of it was already dropped for an unresolved
// workspace no retry can apply the payment, so it is errUnresolvedTenant and
// acknowledged; first_paid_at is not lost, an active snapshot sets it.
// Otherwise subscription_created may not have landed yet, and the error makes
// the provider retry this one after it.
func (s *Service) unknownSubscriptionPayment(ctx context.Context, subID string) error {
	dropped, err := s.d.Repo.HasUnresolvedSubscriptionEvent(ctx, s.provider(), subID, subscriptionEvents)
	if err != nil {
		return err
	}
	if dropped {
		return fmt.Errorf("%w (payment for subscription %s)", errUnresolvedTenant, subID)
	}
	return fmt.Errorf("payment for unknown subscription %s: %w", subID, sql.ErrNoRows)
}

type applyResult struct {
	outcome  string
	tenantID string
	// changed reports whether the mirror changed (for reconcile counting).
	changed bool
}

// applySnapshot mirrors one subscription snapshot: the shared path for
// webhooks and the reconcile sweep. The provider's updated_at makes it
// last-write-wins, so out-of-order and repeated snapshots are harmless.
func (s *Service) applySnapshot(ctx context.Context, snap billingdom.Subscription, custom map[string]string) (applyResult, error) {
	var (
		res          applyResult
		notices      []notice
		entitlements bool
	)
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, notices, entitlements = applyResult{}, nil, false
		prev, err := s.d.Repo.GetSubscriptionByProviderID(ctx, tx, s.provider(), snap.ID, true)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		isNew := prev == nil
		if res.tenantID, err = s.resolveTenant(ctx, prev, custom, snap.ID); err != nil {
			return err
		}
		if !isNew && !snap.UpdatedAt.After(prev.ProviderUpdatedAt) {
			res.outcome = models.WebhookOutcomeStale
			return nil
		}
		sub, before, err := s.saveSnapshot(ctx, tx, prev, res.tenantID, snap, custom)
		if err != nil {
			return err
		}
		res.changed = true
		res.outcome = models.WebhookOutcomeApplied

		moved, err := s.followPlan(ctx, tx, sub, before, isNew)
		if err != nil {
			return err
		}
		entitlements = moved
		ns, err := s.transitions(ctx, tx, sub, before, isNew, moved)
		if err != nil {
			return err
		}
		notices = ns
		return nil
	})
	if err != nil {
		return res, err
	}
	if res.outcome == models.WebhookOutcomeApplied {
		s.publish(ctx, res.tenantID, entitlements)
		s.emit(ctx, notices)
	}
	return res, nil
}

// resolveTenant finds the subscription's workspace: the mirror's for a known
// subscription, else the checkout's custom data. A missing or deleted
// workspace is errUnresolvedTenant.
func (s *Service) resolveTenant(ctx context.Context, prev *models.BillingSubscription, custom map[string]string, subID string) (string, error) {
	tenantID := custom["tenant_id"]
	if prev != nil {
		tenantID = prev.TenantID
	}
	tenant, err := s.d.Tenants.GetByID(ctx, tenantID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && tenant.DeletedAt != nil) || tenantID == "" {
		return "", fmt.Errorf("%w (tenant %q, subscription %s)", errUnresolvedTenant, tenantID, subID)
	}
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

// saveSnapshot inserts or updates the mirror row and returns it with the state
// it had before.
func (s *Service) saveSnapshot(ctx context.Context, tx bun.IDB, prev *models.BillingSubscription, tenantID string, snap billingdom.Subscription, custom map[string]string) (*models.BillingSubscription, models.BillingSubscription, error) {
	variants, err := s.variantMap(ctx)
	if err != nil {
		return nil, models.BillingSubscription{}, err
	}
	sub := prev
	if sub == nil {
		id, err := models.NewID()
		if err != nil {
			return nil, models.BillingSubscription{}, err
		}
		sub = &models.BillingSubscription{ID: id, TenantID: tenantID, Provider: s.provider(), ProviderSubscriptionID: snap.ID, CreatedAt: s.now()}
		if acc := custom["account_id"]; acc != "" {
			sub.AccountID = new(acc)
		}
	}
	before := *sub
	s.copySnapshot(sub, snap, variants)
	sub.UpdatedAt = s.now()
	if prev == nil {
		err = s.d.Repo.InsertSubscription(ctx, tx, sub)
	} else {
		err = s.d.Repo.UpdateSubscription(ctx, tx, sub)
	}
	return sub, before, err
}

// copySnapshot writes the provider's view onto the mirror row.
func (s *Service) copySnapshot(sub *models.BillingSubscription, snap billingdom.Subscription, variants billingdom.VariantMap) {
	sub.ProviderCustomerID = snap.CustomerID
	sub.ProviderOrderID = snap.OrderID
	sub.ProviderVariantID = snap.VariantID
	sub.Status = snap.Status
	sub.TestMode = snap.TestMode
	sub.RenewsAt, sub.EndsAt, sub.TrialEndsAt = snap.RenewsAt, snap.EndsAt, snap.TrialEndsAt
	sub.CardBrand, sub.CardLastFour = snap.CardBrand, snap.CardLastFour
	sub.ProviderUpdatedAt = snap.UpdatedAt
	sub.TierVersionPriceID = nil
	if price, ok := variants.Price(snap.VariantID); ok {
		sub.TierVersionPriceID = new(price)
	}
	// "active" means at least one charge went through; it also covers a lost
	// subscription_payment_success webhook.
	if snap.Status == models.SubscriptionStatusActive && sub.FirstPaidAt == nil {
		sub.FirstPaidAt = new(s.now())
	}
}

// followPlan moves the workspace onto the version the subscription pays for,
// when the subscription is the one the workspace follows and its price
// changed. A change that matches a pending scheduled downgrade is left to the
// scheduled job; any other change cancels that downgrade. It reports whether
// the tier moved.
func (s *Service) followPlan(ctx context.Context, tx bun.IDB, sub *models.BillingSubscription, before models.BillingSubscription, isNew bool) (bool, error) {
	if sub.TierVersionPriceID == nil {
		if sub.ProviderVariantID != "" {
			slog.ErrorContext(ctx, "billing: subscription variant is not mapped to a price; tier unchanged (map it in Harbor)", logging.AttrComponent, "billing",
				"tenant_id", sub.TenantID, "variant_id", sub.ProviderVariantID)
		}
		return false, nil
	}
	newPrice := *sub.TierVersionPriceID
	if !isNew && before.TierVersionPriceID != nil && *before.TierVersionPriceID == newPrice {
		return false, nil
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, tx, sub.TenantID)
	if err != nil {
		return false, err
	}
	if live := s.liveSubscription(subs); live == nil || live.ID != sub.ID {
		return false, nil // an older duplicate, or no longer live
	}
	if !isNew {
		scheduled, err := s.settleOpenChange(ctx, tx, sub.ID, newPrice)
		if err != nil || scheduled {
			return false, err
		}
	}

	prices, err := s.d.Repo.Prices(ctx, compact(newPrice, deref(before.TierVersionPriceID)))
	if err != nil {
		return false, err
	}
	target, ok := prices[newPrice]
	if !ok {
		return false, fmt.Errorf("billing: mapped price %s does not exist", newPrice)
	}
	if cur, err := s.d.Assignments.CoveringAt(ctx, sub.TenantID, s.now()); err == nil && cur.TierVersionID == target.TierVersionID {
		return false, nil // already there (an in-app change reassigned first)
	}
	reason := reassignReason(isNew, prices[deref(before.TierVersionPriceID)], target)
	return s.d.Assignments.ReassignTx(ctx, tx, sub.TenantID, target.TierID, &target.TierVersionID, reason, s.now())
}

// settleOpenChange handles a pending scheduled change when the subscription's
// price moved: it reports true when the move is that change (its job moves the
// tier at the boundary), and cancels it otherwise.
func (s *Service) settleOpenChange(ctx context.Context, tx bun.IDB, subID, newPrice string) (bool, error) {
	change, err := s.d.Repo.OpenScheduledChange(ctx, tx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if change.ToTierVersionPriceID == newPrice {
		return true, nil
	}
	_, err = s.d.Repo.SetScheduledChangeStatus(ctx, tx, change.ID, []string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied},
		models.ScheduledChangeCancelled, "plan changed at the provider", s.now())
	return false, err
}

// reassignReason names a tier move: the first subscription, or an upgrade or
// downgrade by price rank (an unknown previous price counts as an upgrade).
func reassignReason(isNew bool, from, to billingdom.Price) string {
	switch {
	case isNew:
		return models.AssignmentReasonSubscriptionStarted
	case from.ID != "" && billingdom.Direction(from, to) == billingdom.DirectionDowngrade:
		return models.AssignmentReasonDowngrade
	default:
		return models.AssignmentReasonUpgrade
	}
}

// transitions turns a status change into owner notices and side effects. Emails
// and the teardown are enqueued in the transaction; notices are emitted after.
func (s *Service) transitions(ctx context.Context, tx bun.Tx, sub *models.BillingSubscription, before models.BillingSubscription, isNew, moved bool) ([]notice, error) {
	n := func(typ string, level models.NotificationLevel, title, body string) notice {
		return notice{tenantID: sub.TenantID, subID: sub.ID, typ: typ, episode: strconv.FormatInt(sub.ProviderUpdatedAt.UnixNano(), 10), level: level, title: title, body: body}
	}
	var out []notice
	if isNew {
		out = append(out, n("subscription_started", models.NotificationLevelSuccess, "Your plan is active", "Thanks for subscribing. Your workspace is ready."))
	}
	// Only a tier that actually moved is a plan change: a price change the
	// scheduled job or a reverted downgrade made leaves the plan as it was.
	if !isNew && moved {
		out = append(out, n("plan_changed", models.NotificationLevelInfo, "Your plan changed", "Your workspace's plan was updated."))
	}
	if !isNew && before.Status == sub.Status {
		return out, nil
	}
	more, err := s.statusNotices(ctx, tx, sub, before.Status, n)
	return append(out, more...), err
}

// statusNotices turns the new status into notices and its side effects.
func (s *Service) statusNotices(ctx context.Context, tx bun.Tx, sub *models.BillingSubscription, beforeStatus string, n func(typ string, level models.NotificationLevel, title, body string) notice) ([]notice, error) {
	var out []notice
	switch sub.Status {
	case models.SubscriptionStatusPastDue:
		out = append(out, n("payment_failed", models.NotificationLevelWarning, "Your payment didn't go through", "We'll retry the charge. Update your card to avoid an interruption."))
		if err := s.emailPaymentFailed(ctx, tx, sub); err != nil {
			return nil, err
		}
	case models.SubscriptionStatusUnpaid:
		out = append(out, n("payment_failed", models.NotificationLevelError, "Your subscription is unpaid", "Payment retries have run out. Update your card in the billing portal."))
	case models.SubscriptionStatusPaused:
		out = append(out, n("paused", models.NotificationLevelInfo, "Your subscription is paused", "Payments are paused for this workspace."))
	case models.SubscriptionStatusCancelled:
		body := "Your subscription was cancelled."
		if sub.EndsAt != nil {
			body = "Your subscription was cancelled. It stays active until " + sub.EndsAt.Format("2 Jan 2006") + "."
		}
		out = append(out, n("cancelled", models.NotificationLevelInfo, "Subscription cancelled", body))
		if err := s.cancelOpenChange(ctx, tx, sub.ID, "subscription cancelled"); err != nil {
			return nil, err
		}
	case models.SubscriptionStatusExpired:
		ns, err := s.expiredNotice(ctx, tx, sub, n)
		if err != nil {
			return nil, err
		}
		out = append(out, ns)
	case models.SubscriptionStatusActive, models.SubscriptionStatusOnTrial:
		switch beforeStatus {
		case models.SubscriptionStatusPastDue, models.SubscriptionStatusUnpaid, models.SubscriptionStatusPaused, models.SubscriptionStatusCancelled:
			out = append(out, n("payment_recovered", models.NotificationLevelSuccess, "Your subscription is back to normal", "Thanks, everything is up to date."))
		}
	}
	return out, nil
}

// emailPaymentFailed queues the payment-failed email to every owner, once per
// provider update.
func (s *Service) emailPaymentFailed(ctx context.Context, tx bun.Tx, sub *models.BillingSubscription) error {
	owners, err := s.d.Users.ListOwnersByTenant(ctx, sub.TenantID)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s:%d", templates.KeyBillingPaymentFailed, sub.ID, sub.ProviderUpdatedAt.Unix())
	for _, o := range owners {
		if err := s.d.Jobs.EnqueueBillingEmailTx(ctx, tx.Tx, o.ID, sub.TenantID, templates.KeyBillingPaymentFailed, key+":"+o.ID); err != nil {
			return err
		}
	}
	return nil
}

// expiredNotice ends a subscription: any pending downgrade is cancelled, and a
// trial that never paid also releases its Zernio profile.
func (s *Service) expiredNotice(ctx context.Context, tx bun.Tx, sub *models.BillingSubscription, n func(typ string, level models.NotificationLevel, title, body string) notice) (notice, error) {
	if err := s.cancelOpenChange(ctx, tx, sub.ID, "subscription expired"); err != nil {
		return notice{}, err
	}
	if sub.FirstPaidAt != nil {
		return n("expired", models.NotificationLevelError, "Your subscription has ended", "Choose a plan to keep using this workspace."), nil
	}
	if err := s.teardownUnpaid(ctx, tx, sub); err != nil {
		return notice{}, err
	}
	return n("trial_ended_unpaid", models.NotificationLevelError, "Your trial has ended", "Choose a plan to keep using this workspace and reconnect your social accounts."), nil
}

func (s *Service) cancelOpenChange(ctx context.Context, tx bun.IDB, subID, why string) error {
	change, err := s.d.Repo.OpenScheduledChange(ctx, tx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.d.Repo.SetScheduledChangeStatus(ctx, tx, change.ID, []string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied}, models.ScheduledChangeCancelled, why, s.now())
	return err
}

// teardownUnpaid releases the Zernio profile of a workspace whose trial ended
// without a payment, unless another subscription still entitles it. With
// BILLING_TRIAL_TEARDOWN_LIVE off it only reports.
func (s *Service) teardownUnpaid(ctx context.Context, tx bun.Tx, sub *models.BillingSubscription) error {
	if sub.TeardownAt != nil {
		return nil
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, tx, sub.TenantID)
	if err != nil {
		return err
	}
	if s.liveSubscription(subs) != nil {
		return nil
	}
	if !s.d.Settings.TrialTeardownLive {
		trialTeardownsDryRun.Add(1)
		slog.InfoContext(ctx, "billing: unpaid trial ended; teardown is a dry run (BILLING_TRIAL_TEARDOWN_LIVE=false)", logging.AttrComponent, "billing", "tenant_id", sub.TenantID, "subscription_id", sub.ID)
		return nil
	}
	if err := s.d.Jobs.EnqueueTrialTeardownTx(ctx, tx.Tx, sub.TenantID); err != nil {
		return err
	}
	sub.TeardownAt = new(s.now())
	trialTeardownsQueued.Add(1)
	return s.d.Repo.UpdateSubscription(ctx, tx, sub)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// compact drops empty ids.
func compact(ids ...string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
