package billing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

func TestWebhookStartsSubscriptionAndLiftsGate(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, _, accountID := h.workspace("a")

	if g, _ := h.svc.Gated(ctx, tenantID); !g {
		t.Fatal("new workspace not gated")
	}
	snap := h.sub("s1", proVariant, models.SubscriptionStatusOnTrial, h.now)
	custom := map[string]string{"tenant_id": tenantID, "account_id": accountID}
	if err := h.webhook("subscription_created", snap, custom); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); g {
		t.Fatal("gate still on after the subscription")
	}
	if got := h.currentVersion(tenantID); got != h.proPrice.TierVersionID+"/"+models.AssignmentReasonSubscriptionStarted {
		t.Fatalf("assignment = %s", got)
	}
	m := h.mirror("s1")
	if m.Status != models.SubscriptionStatusOnTrial || m.TierVersionPriceID == nil || *m.TierVersionPriceID != h.proPrice.ID || m.AccountID == nil || m.FirstPaidAt != nil {
		t.Fatalf("mirror = %+v", m)
	}
	if !h.notifier.has("subscription_started") {
		t.Fatal("owners not notified")
	}
	if eligible, _ := h.svc.TrialEligible(ctx, accountID); eligible {
		t.Fatal("account still trial-eligible after its free month")
	}

	// A replay of the same body is a no-op; an older snapshot is stale.
	if err := h.webhook("subscription_created", snap, custom); err != nil {
		t.Fatal(err)
	}
	older := snap
	older.Status, older.UpdatedAt = models.SubscriptionStatusExpired, h.now.Add(-time.Minute)
	if err := h.webhook("subscription_updated", older, nil); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusOnTrial {
		t.Fatal("stale snapshot applied")
	}

	// The other mode is ignored; a bad signature is refused.
	other := snap
	other.TestMode, other.UpdatedAt, other.Status = false, h.now.Add(time.Hour), models.SubscriptionStatusExpired
	if err := h.webhook("subscription_updated", other, nil); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusOnTrial {
		t.Fatal("other-mode snapshot applied")
	}
	if err := h.svc.HandleWebhook(ctx, "bad", []byte(`{}`)); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("bad signature err = %v", err)
	}
}

func TestWebhookUnknownTenantIsRecordedNotRetried(t *testing.T) {
	h := newHarness(t)
	snap := h.sub("s1", proVariant, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription_created", snap, map[string]string{"tenant_id": "nope"}); err != nil {
		t.Fatalf("unresolved tenant should be acknowledged: %v", err)
	}
	items, _, err := h.svc.Attention(t.Context(), 50, "")
	if err != nil || len(items) != 1 || items[0].Reason != AttentionUnresolvedWebhook {
		t.Fatalf("attention = %+v %v", items, err)
	}
}

func TestPastDueNotifiesAndKeepsTier(t *testing.T) {
	h := newHarness(t)
	tenantID, userID, _ := h.workspace("a")
	snap := h.sub("s1", proVariant, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription_created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").FirstPaidAt == nil {
		t.Fatal("active subscription not marked paid")
	}
	before := h.currentVersion(tenantID)
	snap.Status, snap.UpdatedAt = models.SubscriptionStatusPastDue, h.now.Add(time.Minute)
	if err := h.webhook("subscription_updated", snap, nil); err != nil {
		t.Fatal(err)
	}
	if h.currentVersion(tenantID) != before {
		t.Fatal("dunning changed the tier")
	}
	if len(h.jobs.emails) != 1 || h.jobs.emails[0].userID != userID || !strings.HasPrefix(h.jobs.emails[0].key, "billing_payment_failed") {
		t.Fatalf("emails = %+v", h.jobs.emails)
	}
	if !h.notifier.has("payment_failed") {
		t.Fatal("no payment_failed notice")
	}
	items, _, _ := h.svc.Attention(t.Context(), 50, "")
	if len(items) != 1 || items[0].Reason != AttentionPastDue {
		t.Fatalf("attention = %+v", items)
	}
	if g, _ := h.svc.Gated(t.Context(), tenantID); g {
		t.Fatal("past_due re-gated")
	}
}

func TestUnpaidTrialExpiryTearsDown(t *testing.T) {
	for _, live := range []bool{false, true} {
		h := newHarness(t, func(s *billingdom.Settings) { s.TrialTeardownLive = live })
		tenantID, _, _ := h.workspace("a")
		snap := h.sub("s1", proVariant, models.SubscriptionStatusOnTrial, h.now)
		if err := h.webhook("subscription_created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
			t.Fatal(err)
		}
		snap.Status, snap.UpdatedAt = models.SubscriptionStatusExpired, h.now.Add(time.Minute)
		if err := h.webhook("subscription_expired", snap, nil); err != nil {
			t.Fatal(err)
		}
		m := h.mirror("s1")
		if live {
			if len(h.jobs.teardowns) != 1 || m.TeardownAt == nil {
				t.Fatalf("live teardown: %v %v", h.jobs.teardowns, m.TeardownAt)
			}
		} else if len(h.jobs.teardowns) != 0 || m.TeardownAt != nil {
			t.Fatalf("dry-run teardown enqueued: %v", h.jobs.teardowns)
		}
		if !h.notifier.has("trial_ended_unpaid") {
			t.Fatal("no trial_ended_unpaid notice")
		}
		items, _, _ := h.svc.Attention(t.Context(), 50, "")
		if len(items) != 1 || items[0].Reason != AttentionExpiredNeverPaid {
			t.Fatalf("attention = %+v", items)
		}
	}
}

func TestPaidThenExpiredDoesNotTearDown(t *testing.T) {
	h := newHarness(t, func(s *billingdom.Settings) { s.TrialTeardownLive = true })
	tenantID, _, _ := h.workspace("a")
	snap := h.sub("s1", proVariant, models.SubscriptionStatusOnTrial, h.now)
	if err := h.webhook("subscription_created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	pay, _ := json.Marshal(billingdom.Event{Name: "subscription_payment_success", Kind: billingdom.EventPayment, TestMode: true, ObjectID: "inv1", PaymentSubscriptionID: "s1", PaymentAmountMinor: 2900})
	if err := h.svc.HandleWebhook(t.Context(), "ok", pay); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").FirstPaidAt == nil {
		t.Fatal("payment not recorded")
	}
	snap.Status, snap.UpdatedAt = models.SubscriptionStatusExpired, h.now.Add(time.Minute)
	if err := h.webhook("subscription_expired", snap, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs.teardowns) != 0 || !h.notifier.has("expired") {
		t.Fatalf("paid expiry tore down: %v", h.jobs.teardowns)
	}
}

func TestReconcileFixesDroppedWebhook(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	snap := h.sub("s1", proVariant, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription_created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	// The provider moves on; its webhook never arrives.
	h.sub("s1", proVariant, models.SubscriptionStatusPastDue, h.now.Add(time.Hour))
	if err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.mirror("s1").Status; got != models.SubscriptionStatusPastDue {
		t.Fatalf("reconciled status = %s", got)
	}
	if n, err := h.svc.ResyncTenant(t.Context(), tenantID); err != nil || n != 0 {
		t.Fatalf("resync of a synced tenant = %d %v", n, err)
	}
}

func TestCheckoutFreeMonth(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, accountID := h.workspace("a")
	buyer := Buyer{TenantID: tenantID, UserID: userID, AccountID: accountID, Email: "a@example.com"}

	// No tier marked: charged at checkout.
	res, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "pro"})
	if err != nil || res.Checkout == nil || res.Checkout.Trial || !h.provider.checkouts[0].SkipTrial {
		t.Fatalf("unmarked checkout = %+v %v", res, err)
	}
	if _, err := h.svc.SetFreeTrialTier(ctx, "max", "launch"); !errors.Is(err, ErrTrialVariantMissing) {
		t.Fatalf("max without a trial variant: %v", err)
	}
	offer, err := h.svc.SetFreeTrialTier(ctx, "pro", "launch")
	if err != nil || offer.TierID != "pro" || offer.TrialDays != 30 {
		t.Fatalf("mark pro = %+v %v", offer, err)
	}
	res, err = h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: h.proPrice.TierVersionID})
	if err != nil || res.Checkout == nil || !res.Checkout.Trial || h.provider.checkouts[1].SkipTrial {
		t.Fatalf("marked checkout = %+v %v", res, err)
	}
	co := h.provider.checkouts[1]
	if co.VariantID != proVariant || co.Custom["tenant_id"] != tenantID || co.Custom["account_id"] != accountID ||
		co.RedirectURL != "https://app/plans?billing=return&workspace="+tenantID || !co.TestMode {
		t.Fatalf("checkout request = %+v", co)
	}
	// Max is not the marked tier: charged at checkout.
	res, _ = h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "max"})
	if res.Checkout == nil || res.Checkout.Trial {
		t.Fatalf("max checkout = %+v", res)
	}
	flags, err := h.svc.PricingFlags(ctx)
	if err != nil || flags.FreeTrialTierID != "pro" || flags.TrialDays != 30 || !flags.MappedPrices[h.proPrice.ID] {
		t.Fatalf("pricing flags = %+v %v", flags, err)
	}

	// Once the account had its trial, the next checkout charges.
	if err := h.webhook("subscription_created", h.sub("s1", proVariant, models.SubscriptionStatusOnTrial, h.now), map[string]string{"tenant_id": tenantID, "account_id": accountID}); err != nil {
		t.Fatal(err)
	}
	other, _, _ := h.workspace("b")
	res, err = h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: other, UserID: "u-b", AccountID: accountID}, TierID: "pro"})
	if err != nil || res.Checkout == nil || res.Checkout.Trial {
		t.Fatalf("second workspace checkout = %+v %v", res, err)
	}

	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "nope"}); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown tier: %v", err)
	}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "pro", PriceID: h.maxPrice.ID}); !errors.Is(err, ErrPriceMismatch) {
		t.Fatalf("price mismatch: %v", err)
	}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "default"}); !errors.Is(err, ErrTierNotAvailable) {
		t.Fatalf("default tier: %v", err)
	}
}

func subscribe(t *testing.T, h *harness, tenantID, status string) {
	t.Helper()
	if err := h.webhook("subscription_created", h.sub("s1", proVariant, status, h.now), map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeNow(t *testing.T) {
	h := newHarness(t)
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusActive)
	h.now = h.now.Add(time.Minute)
	res, err := h.svc.ChangePlan(t.Context(), PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"})
	if err != nil || res.Checkout != nil {
		t.Fatalf("upgrade = %+v %v", res, err)
	}
	if len(h.provider.changes) != 1 || h.provider.changes[0].variantID != maxVariant || !h.provider.changes[0].opts.InvoiceNow || h.provider.changes[0].opts.EndTrialNow {
		t.Fatalf("provider change = %+v", h.provider.changes)
	}
	if got := h.currentVersion(tenantID); got != h.maxPrice.TierVersionID+"/"+models.AssignmentReasonUpgrade {
		t.Fatalf("assignment = %s", got)
	}
	// The later webhook for the same change does not move the tier again.
	if err := h.webhook("subscription_updated", h.provider.subs["s1"], nil); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); got != h.maxPrice.TierVersionID+"/"+models.AssignmentReasonUpgrade {
		t.Fatalf("assignment after webhook = %s", got)
	}
}

func TestTierChangeDuringTrialEndsTrial(t *testing.T) {
	h := newHarness(t)
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusOnTrial)
	h.now = h.now.Add(time.Minute)
	if _, err := h.svc.ChangePlan(t.Context(), PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"}); err != nil {
		t.Fatal(err)
	}
	if c := h.provider.changes[0]; !c.opts.EndTrialNow || !c.opts.InvoiceNow {
		t.Fatalf("trial change opts = %+v", c.opts)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusActive {
		t.Fatal("mirror not updated from the provider answer")
	}

	h.provider.changeErr = &permanentErr{}
	tenant2, user2, _ := h.workspace("b")
	if err := h.webhook("subscription_created", h.sub("s2", proVariant, models.SubscriptionStatusOnTrial, h.now), map[string]string{"tenant_id": tenant2}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ChangePlan(t.Context(), PlanRequest{Buyer: Buyer{TenantID: tenant2, UserID: user2}, TierID: "max"}); !errors.Is(err, ErrTrialChangeUnavailable) {
		t.Fatalf("refused trial change: %v", err)
	}
}

type permanentErr struct{}

func (*permanentErr) Error() string   { return "HTTP 422" }
func (*permanentErr) Retryable() bool { return false }

func TestScheduledDowngrade(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	if err := h.webhook("subscription_created", h.sub("s1", maxVariant, models.SubscriptionStatusActive, h.now), map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	buyer := Buyer{TenantID: tenantID, UserID: userID}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "pro"}); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.changes) != 0 || len(h.jobs.steps) != 2 {
		t.Fatalf("downgrade touched the provider early: %+v steps=%v", h.provider.changes, h.jobs.steps)
	}
	st, err := h.svc.PlanState(ctx, tenantID, "")
	if err != nil || st.ScheduledChange == nil || st.ScheduledChange.Direction != billingdom.DirectionDowngrade || st.ScheduledChange.TierID != "pro" {
		t.Fatalf("plan state = %+v %v", st, err)
	}
	changeID := strings.Split(h.jobs.steps[0], "@")[0]
	maxVersion := h.maxPrice.TierVersionID + "/" + models.AssignmentReasonSubscriptionStarted

	// Not due yet: nothing happens.
	if err := h.svc.RunScheduledChange(ctx, changeID); err != nil || len(h.provider.changes) != 0 {
		t.Fatalf("early step: %v %+v", err, h.provider.changes)
	}
	// T-1h: the provider switches without proration; the tier stays.
	h.now = st.ScheduledChange.EffectiveAt.Add(-30 * time.Minute)
	if err := h.svc.RunScheduledChange(ctx, changeID); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.changes) != 1 || !h.provider.changes[0].opts.NoProration || h.provider.changes[0].variantID != proVariant {
		t.Fatalf("provider step = %+v", h.provider.changes)
	}
	if got := h.currentVersion(tenantID); got != maxVersion {
		t.Fatalf("tier moved early: %s", got)
	}
	// T: the tier moves.
	h.now = st.ScheduledChange.EffectiveAt.Add(time.Minute)
	if err := h.svc.RunScheduledChange(ctx, changeID); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); got != h.proPrice.TierVersionID+"/"+models.AssignmentReasonDowngrade {
		t.Fatalf("assignment after the boundary = %s", got)
	}
	if st, _ := h.svc.PlanState(ctx, tenantID, ""); st.ScheduledChange != nil {
		t.Fatal("applied change still shown")
	}
}

func TestKeepCurrentPlanCancelsDowngrade(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	if err := h.webhook("subscription_created", h.sub("s1", maxVariant, models.SubscriptionStatusActive, h.now), map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	buyer := Buyer{TenantID: tenantID, UserID: userID}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "pro"}); err != nil {
		t.Fatal(err)
	}
	st, _ := h.svc.PlanState(ctx, tenantID, "")
	changeID := strings.Split(h.jobs.steps[0], "@")[0]
	h.now = st.ScheduledChange.EffectiveAt.Add(-30 * time.Minute)
	if err := h.svc.RunScheduledChange(ctx, changeID); err != nil {
		t.Fatal(err)
	}
	// Choosing the current plan after the provider switched reverts it.
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "max"}); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.changes) != 2 || h.provider.changes[1].variantID != maxVariant || !h.provider.changes[1].opts.NoProration {
		t.Fatalf("revert = %+v", h.provider.changes)
	}
	if st, _ := h.svc.PlanState(ctx, tenantID, ""); st.ScheduledChange != nil {
		t.Fatal("downgrade still pending")
	}
	h.now = st.ScheduledChange.EffectiveAt.Add(time.Minute)
	if err := h.svc.RunScheduledChange(ctx, changeID); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); !strings.HasPrefix(got, h.maxPrice.TierVersionID) {
		t.Fatalf("cancelled downgrade still landed: %s", got)
	}
}

func TestNotChangeableAndBillingView(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	if v, err := h.svc.Billing(ctx, tenantID); err != nil || v.Subscription != nil || v.Portal || !v.TestMode {
		t.Fatalf("never bought = %+v %v", v, err)
	}
	if _, _, err := h.svc.Portal(ctx, tenantID); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("portal without subscription: %v", err)
	}
	subscribe(t, h, tenantID, models.SubscriptionStatusPastDue)
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"}); !errors.Is(err, ErrNotChangeable) {
		t.Fatalf("past_due change: %v", err)
	}
	v, err := h.svc.Billing(ctx, tenantID)
	if err != nil || v.Subscription == nil || v.Subscription.Status != models.SubscriptionStatusPastDue || v.Subscription.Card.LastFour != "4242" ||
		v.Subscription.Price.Amount != 2900 || v.Subscription.Price.Period != "month" || !v.Portal {
		t.Fatalf("billing view = %+v %v", v, err)
	}
	url, exp, err := h.svc.Portal(ctx, tenantID)
	if err != nil || url != "https://portal/s1" || !exp.After(h.now) {
		t.Fatalf("portal = %s %v %v", url, exp, err)
	}
}

func TestCutover(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusActive)
	if _, err := h.svc.InvalidateTestSubscriptions(ctx, true); !errors.Is(err, ErrStillTestMode) {
		t.Fatalf("cutover in test mode: %v", err)
	}
	h.svc.d.Settings.TestMode = false
	ids, err := h.svc.InvalidateTestSubscriptions(ctx, true)
	if err != nil || len(ids) != 1 || ids[0] != tenantID {
		t.Fatalf("dry run = %v %v", ids, err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); g {
		t.Fatal("dry run re-gated")
	}
	if _, err := h.svc.InvalidateTestSubscriptions(ctx, false); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); !g {
		t.Fatal("cutover did not re-gate")
	}
	if len(h.jobs.emails) != 1 || h.jobs.emails[0].userID != userID || h.jobs.emails[0].key != "billing_reentry_required" {
		t.Fatalf("emails = %+v", h.jobs.emails)
	}
	if h.mirror("s1").InvalidatedReason != models.InvalidatedReasonModeCutover {
		t.Fatal("not invalidated")
	}
	if ids, _ := h.svc.InvalidateTestSubscriptions(ctx, true); len(ids) != 0 {
		t.Fatalf("second run still finds %v", ids)
	}
}

func TestSetBillingRequiredAndStatus(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, _, _ := h.workspace("a")
	if err := h.svc.SetBillingRequired(ctx, tenantID, false, ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("no reason: %v", err)
	}
	if err := h.svc.SetBillingRequired(ctx, tenantID, false, "design partner"); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); g {
		t.Fatal("comped workspace still gated")
	}
	tb, err := h.svc.TenantBilling(ctx, tenantID)
	if err != nil || tb.BillingRequired || len(tb.Owners) != 1 || !tb.Owners[0].TrialEligible {
		t.Fatalf("tenant billing = %+v %v", tb, err)
	}
	tenant, _ := h.svc.d.Tenants.GetByID(ctx, tenantID)
	if st, _ := h.svc.BillingStatus(ctx, tenant); st != "not_required" {
		t.Fatalf("status = %s", st)
	}
}
