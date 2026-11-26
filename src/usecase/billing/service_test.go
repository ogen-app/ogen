package billing

import (
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
	snap := h.sub("s1", proProduct, models.SubscriptionStatusOnTrial, h.now)
	meta := map[string]string{"tenant_id": tenantID, "account_id": accountID}
	if err := h.webhook("subscription.created", snap, meta); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); g {
		t.Fatal("gate still on after the subscription")
	}
	if got := h.currentVersion(tenantID); got != h.proPrice.TierVersionID+"/"+models.AssignmentReasonSubscriptionStarted {
		t.Fatalf("assignment = %s", got)
	}
	m := h.mirror("s1")
	if m.Status != models.SubscriptionStatusOnTrial || m.TierVersionPriceID == nil || *m.TierVersionPriceID != h.proPrice.ID || m.AccountID == nil || m.FirstPaidAt != nil ||
		m.ProviderProductID != proProduct || m.ProviderVariantID != proProduct || m.ProviderStatus != models.SubscriptionStatusOnTrial || m.Provider != models.BillingProviderPolar {
		t.Fatalf("mirror = %+v", m)
	}
	if !h.notifier.has("subscription_started") {
		t.Fatal("owners not notified")
	}
	if eligible, _ := h.svc.TrialEligible(ctx, accountID); eligible {
		t.Fatal("account still trial-eligible after its free month")
	}

	// An older snapshot is stale.
	older := snap
	older.Status, older.UpdatedAt = models.SubscriptionStatusExpired, h.now.Add(-time.Minute)
	if err := h.webhook("subscription.updated", older, nil); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusOnTrial {
		t.Fatal("stale snapshot applied")
	}

	// The other mode is ignored; a bad signature is refused.
	other := snap
	other.TestMode, other.UpdatedAt, other.Status = false, h.now.Add(time.Hour), models.SubscriptionStatusExpired
	if err := h.webhook("subscription.updated", other, nil); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusOnTrial {
		t.Fatal("other-mode snapshot applied")
	}
	if err := h.svc.HandleWebhook(ctx, billingdom.WebhookHeaders{ID: "x", Signature: "bad"}, []byte(`{}`)); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("bad signature err = %v", err)
	}
}

func TestWebhookRedeliveryIsRecognisedByEventID(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now)
	snap.Metadata = map[string]string{"tenant_id": tenantID}
	ev := billingdom.Event{Name: "subscription.created", Kind: billingdom.EventSubscription, TestMode: true, ObjectID: "s1", Subscription: &snap}
	if err := h.deliverAs("evt_same", ev); err != nil {
		t.Fatal(err)
	}
	// The same event id with a changed body (a newer snapshot) is a
	// redelivery: nothing is applied.
	later := snap
	later.Status, later.UpdatedAt = models.SubscriptionStatusPastDue, h.now.Add(time.Minute)
	ev.Subscription = &later
	if err := h.deliverAs("evt_same", ev); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusActive {
		t.Fatal("a redelivery was applied")
	}
	n, err := h.db.NewSelect().TableExpr("billing_webhook_events").Where("provider_event_id = ?", "evt_same").Count(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("event rows = %d %v", n, err)
	}
}

func TestSnapshotWithoutOgenStatus(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	meta := map[string]string{"tenant_id": tenantID}
	// A checkout still being paid (Polar incomplete) mirrors nothing.
	snap := h.sub("s1", proProduct, "", h.now)
	snap.ProviderStatus = "incomplete"
	if err := h.webhook("subscription.created", snap, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.GetSubscriptionByProviderID(t.Context(), nil, models.BillingProviderPolar, "s1", false); err == nil {
		t.Fatal("an incomplete subscription was mirrored")
	}
	// Once mirrored, a status this build does not know keeps the status it
	// had, and records the raw one.
	snap = h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now.Add(time.Minute))
	if err := h.webhook("subscription.active", snap, meta); err != nil {
		t.Fatal(err)
	}
	snap.Status, snap.ProviderStatus, snap.UpdatedAt = "", "something_new", h.now.Add(2*time.Minute)
	if err := h.webhook("subscription.updated", snap, meta); err != nil {
		t.Fatal(err)
	}
	if m := h.mirror("s1"); m.Status != models.SubscriptionStatusActive || m.ProviderStatus != "something_new" {
		t.Fatalf("mirror = %s / %s", m.Status, m.ProviderStatus)
	}
}

func TestWebhookUnknownTenantIsRecordedNotRetried(t *testing.T) {
	h := newHarness(t)
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": "nope"}); err != nil {
		t.Fatalf("unresolved tenant should be acknowledged: %v", err)
	}
	items, _, err := h.svc.Attention(t.Context(), 50, "")
	if err != nil || len(items) != 1 || items[0].Reason != AttentionUnresolvedWebhook {
		t.Fatalf("attention = %+v %v", items, err)
	}
}

func TestWebhookResolvesTenantFromCustomerExternalID(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now)
	snap.CustomerExternalID = tenantID
	if err := h.webhook("subscription.created", snap, nil); err != nil {
		t.Fatal(err)
	}
	if m := h.mirror("s1"); m.TenantID != tenantID {
		t.Fatalf("tenant = %s", m.TenantID)
	}
}

// paymentWebhook delivers a paid order for a subscription.
func (h *harness) paymentWebhook(orderID, subID string) error {
	return h.deliverAs("evt_"+orderID, billingdom.Event{Name: "order.paid", Kind: billingdom.EventPayment, TestMode: true, ObjectID: orderID,
		PaymentSubscriptionID: subID, PaymentCharged: true})
}

func TestPaymentForUnresolvedSubscriptionIsAcknowledged(t *testing.T) {
	h := newHarness(t)
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": "nope"}); err != nil {
		t.Fatalf("unresolved tenant should be acknowledged: %v", err)
	}
	// No retry can apply the payment: it is recorded and acknowledged, and so
	// is its redelivery.
	for range 2 {
		if err := h.paymentWebhook("o1", "s1"); err != nil {
			t.Fatalf("payment for a dropped subscription should be acknowledged: %v", err)
		}
	}
	items, _, err := h.svc.Attention(t.Context(), 50, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("attention = %+v %v", items, err)
	}
	for _, it := range items {
		if it.Reason != AttentionUnresolvedWebhook {
			t.Fatalf("attention reason = %s", it.Reason)
		}
	}
}

func TestPaymentBeforeSubscriptionIsRetriedForAnHour(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	// A payment that beats the subscription's webhook fails so the provider
	// retries it.
	if err := h.paymentWebhook("o1", "s1"); err == nil || errors.Is(err, errUnresolvedTenant) {
		t.Fatalf("early payment err = %v", err)
	}
	snap := h.sub("s1", proProduct, models.SubscriptionStatusOnTrial, h.now)
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	if err := h.paymentWebhook("o1", "s1"); err != nil {
		t.Fatalf("redelivered payment: %v", err)
	}
	if h.mirror("s1").FirstPaidAt == nil {
		t.Fatal("payment not recorded")
	}

	// One that keeps failing is acknowledged once its first delivery is over
	// an hour old, so the endpoint is never disabled for it.
	if err := h.paymentWebhook("o2", "s-unknown"); err == nil {
		t.Fatal("early payment acknowledged at once")
	}
	h.now = h.now.Add(webhookRetryWindow + time.Minute)
	if err := h.paymentWebhook("o2", "s-unknown"); err != nil {
		t.Fatalf("a delivery past the retry window: %v", err)
	}
}

func TestPastDueNotifiesAndKeepsTier(t *testing.T) {
	h := newHarness(t)
	tenantID, userID, _ := h.workspace("a")
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").FirstPaidAt == nil {
		t.Fatal("active subscription not marked paid")
	}
	before := h.currentVersion(tenantID)
	snap.Status, snap.UpdatedAt = models.SubscriptionStatusPastDue, h.now.Add(time.Minute)
	if err := h.webhook("subscription.past_due", snap, nil); err != nil {
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
	// A malformed or negative page token starts from the top instead of
	// panicking on the slice.
	for _, token := range []string{"-1", "x"} {
		if items, _, err := h.svc.Attention(t.Context(), 50, token); err != nil || len(items) != 1 {
			t.Fatalf("attention with token %q = %+v %v", token, items, err)
		}
	}
	if g, _ := h.svc.Gated(t.Context(), tenantID); g {
		t.Fatal("past_due re-gated")
	}
}

func TestUnpaidTrialExpiryTearsDown(t *testing.T) {
	for _, live := range []bool{false, true} {
		h := newHarness(t, func(s *billingdom.Settings) { s.TrialTeardownLive = live })
		tenantID, _, _ := h.workspace("a")
		snap := h.sub("s1", proProduct, models.SubscriptionStatusOnTrial, h.now)
		if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
			t.Fatal(err)
		}
		snap.Status, snap.UpdatedAt = models.SubscriptionStatusExpired, h.now.Add(time.Minute)
		if err := h.webhook("subscription.updated", snap, nil); err != nil {
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
	snap := h.sub("s1", proProduct, models.SubscriptionStatusOnTrial, h.now)
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	if err := h.paymentWebhook("o1", "s1"); err != nil {
		t.Fatal(err)
	}
	if h.mirror("s1").FirstPaidAt == nil {
		t.Fatal("payment not recorded")
	}
	snap.Status, snap.UpdatedAt = models.SubscriptionStatusExpired, h.now.Add(time.Minute)
	if err := h.webhook("subscription.revoked", snap, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs.teardowns) != 0 || !h.notifier.has("expired") {
		t.Fatalf("paid expiry tore down: %v", h.jobs.teardowns)
	}
}

func TestReconcileFixesDroppedWebhook(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	snap := h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now)
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	// The provider moves on; its webhook never arrives.
	h.sub("s1", proProduct, models.SubscriptionStatusPastDue, h.now.Add(time.Hour))
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
	if err != nil || res.Checkout == nil || res.Checkout.Trial || !h.provider.checkouts[0].SkipTrial || res.Checkout.ExpiresAt.IsZero() {
		t.Fatalf("unmarked checkout = %+v %v", res, err)
	}
	if _, err := h.svc.SetFreeTrialTier(ctx, "max", "launch"); !errors.Is(err, ErrTrialProductMissing) {
		t.Fatalf("max without a trial product: %v", err)
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
	if co.ProductID != proProduct || co.CustomerExternalID != tenantID || co.Metadata["tenant_id"] != tenantID || co.Metadata["account_id"] != accountID ||
		co.SuccessURL != "https://app/plans?billing=return&workspace="+tenantID || !co.TestMode || co.Email != "a@example.com" {
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
	if err := h.webhook("subscription.created", h.sub("s1", proProduct, models.SubscriptionStatusOnTrial, h.now), map[string]string{"tenant_id": tenantID, "account_id": accountID}); err != nil {
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

// lemonSqueezyRow stores a subscription of the provider before Polar.
func (h *harness) lemonSqueezyRow(id, tenantID, accountID string, trial bool) {
	h.t.Helper()
	renews := h.now.Add(10 * 24 * time.Hour)
	row := &models.BillingSubscription{ID: id, TenantID: tenantID, Provider: models.BillingProviderLemonSqueezy, ProviderSubscriptionID: "ls-" + id,
		ProviderVariantID: "101", Status: models.SubscriptionStatusActive, TestMode: true, RenewsAt: &renews, ProviderUpdatedAt: h.now, CreatedAt: h.now, UpdatedAt: h.now}
	if accountID != "" {
		row.AccountID = new(accountID)
	}
	if trial {
		row.TrialEndsAt = &renews
	}
	if err := h.repo.InsertSubscription(h.t.Context(), nil, row); err != nil {
		h.t.Fatal(err)
	}
}

func TestPreviousProviderSubscriptionsAreHistory(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, accountID := h.workspace("a")
	if _, err := h.svc.SetFreeTrialTier(ctx, "pro", "launch"); err != nil {
		t.Fatal(err)
	}
	h.lemonSqueezyRow("ls1", tenantID, accountID, true)

	// The Lemon Squeezy row still counts for the gate until the cutover...
	if g, _ := h.svc.Gated(ctx, tenantID); g {
		t.Fatal("gated before the cutover")
	}
	// ...but is not a subscription Polar can change: choosing a plan checks
	// out, and the Lemon Squeezy trial does not use up the Polar one.
	res, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID, AccountID: accountID}, TierID: "pro"})
	if err != nil || res.Checkout == nil || !res.Checkout.Trial {
		t.Fatalf("plan choice with a Lemon Squeezy row = %+v %v", res, err)
	}
	if v, err := h.svc.Billing(ctx, tenantID); err != nil || v.Subscription != nil {
		t.Fatalf("billing card shows the Lemon Squeezy row: %+v %v", v, err)
	}
	if err := h.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}

func subscribe(t *testing.T, h *harness, tenantID, status string) {
	t.Helper()
	if err := h.webhook("subscription.created", h.sub("s1", proProduct, status, h.now), map[string]string{"tenant_id": tenantID}); err != nil {
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
	if len(h.provider.changes) != 1 || h.provider.changes[0].productID != maxProduct || h.provider.changes[0].opts.Timing != billingdom.ChargeNow || h.provider.changes[0].opts.EndTrialNow {
		t.Fatalf("provider change = %+v", h.provider.changes)
	}
	if got := h.currentVersion(tenantID); got != h.maxPrice.TierVersionID+"/"+models.AssignmentReasonUpgrade {
		t.Fatalf("assignment = %s", got)
	}
	// The later webhook for the same change does not move the tier again.
	if err := h.webhook("subscription.updated", h.provider.subs["s1"], nil); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); got != h.maxPrice.TierVersionID+"/"+models.AssignmentReasonUpgrade {
		t.Fatalf("assignment after webhook = %s", got)
	}
}

type lockedErr struct{}

func (*lockedErr) Error() string   { return "HTTP 409 SubscriptionLocked" }
func (*lockedErr) Retryable() bool { return true }
func (*lockedErr) Locked() bool    { return true }

func TestUpgradeRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	lockedRetryDelay = time.Millisecond
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusActive)
	h.now = h.now.Add(time.Minute)
	req := PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"}
	before := h.currentVersion(tenantID)

	// A declined charge changes nothing.
	h.provider.changeErr = billingdom.ErrPaymentFailed
	if _, err := h.svc.ChangePlan(ctx, req); !errors.Is(err, ErrPaymentFailed) {
		t.Fatalf("declined upgrade: %v", err)
	}
	if h.currentVersion(tenantID) != before {
		t.Fatal("a declined upgrade moved the tier")
	}
	// A subscription the provider already cancelled is not changeable.
	h.provider.changeErr = billingdom.ErrAlreadyCanceled
	if _, err := h.svc.ChangePlan(ctx, req); !errors.Is(err, ErrNotChangeable) {
		t.Fatalf("cancelled at the provider: %v", err)
	}
	// A locked subscription is retried once.
	h.provider.changeErr = nil
	h.provider.changeErrs = []error{&lockedErr{}}
	if _, err := h.svc.ChangePlan(ctx, req); err != nil {
		t.Fatalf("locked once: %v", err)
	}
	if got := h.currentVersion(tenantID); !strings.HasPrefix(got, h.maxPrice.TierVersionID) {
		t.Fatalf("assignment after the retry = %s", got)
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
	if c := h.provider.changes[0]; !c.opts.EndTrialNow || c.opts.Timing != billingdom.ChargeNow {
		t.Fatalf("trial change opts = %+v", c.opts)
	}
	if h.mirror("s1").Status != models.SubscriptionStatusActive {
		t.Fatal("mirror not updated from the provider answer")
	}

	h.provider.changeErr = &permanentErr{}
	tenant2, user2, _ := h.workspace("b")
	if err := h.webhook("subscription.created", h.sub("s2", proProduct, models.SubscriptionStatusOnTrial, h.now), map[string]string{"tenant_id": tenant2}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ChangePlan(t.Context(), PlanRequest{Buyer: Buyer{TenantID: tenant2, UserID: user2}, TierID: "max"}); !errors.Is(err, ErrTrialChangeUnavailable) {
		t.Fatalf("refused trial change: %v", err)
	}
}

type permanentErr struct{}

func (*permanentErr) Error() string   { return "HTTP 422" }
func (*permanentErr) Retryable() bool { return false }

// downgradeToPro starts tenant a on Max and schedules its move to Pro.
func downgradeToPro(t *testing.T, h *harness) (tenantID string, buyer Buyer, effective time.Time) {
	t.Helper()
	tenantID, userID, _ := h.workspace("a")
	if err := h.webhook("subscription.created", h.sub("s1", maxProduct, models.SubscriptionStatusActive, h.now), map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	buyer = Buyer{TenantID: tenantID, UserID: userID}
	if _, err := h.svc.ChangePlan(t.Context(), PlanRequest{Buyer: buyer, TierID: "pro"}); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.PlanState(t.Context(), tenantID, "")
	if err != nil || st.ScheduledChange == nil || st.ScheduledChange.Direction != billingdom.DirectionDowngrade || st.ScheduledChange.TierID != "pro" {
		t.Fatalf("plan state = %+v %v", st, err)
	}
	return tenantID, buyer, st.ScheduledChange.EffectiveAt
}

func TestScheduledDowngradeLandsAtRenewal(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, _, effective := downgradeToPro(t, h)

	// The provider holds the change for the next period; nothing else moves.
	if len(h.provider.changes) != 1 || h.provider.changes[0].opts.Timing != billingdom.NextPeriod || h.provider.changes[0].productID != proProduct {
		t.Fatalf("provider change = %+v", h.provider.changes)
	}
	m := h.mirror("s1")
	if m.PendingProductID != proProduct || m.PendingEffectiveAt == nil || !m.PendingEffectiveAt.Equal(effective) || m.ProviderProductID != maxProduct {
		t.Fatalf("mirror = %+v", m)
	}
	maxVersion := h.maxPrice.TierVersionID + "/" + models.AssignmentReasonSubscriptionStarted
	if got := h.currentVersion(tenantID); got != maxVersion {
		t.Fatalf("tier moved early: %s", got)
	}
	// Picking the same lower plan again changes nothing.
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: "u-a"}, TierID: "pro"}); err != nil || len(h.provider.changes) != 1 {
		t.Fatalf("repeat downgrade: %v %+v", err, h.provider.changes)
	}

	// At renewal the provider applies it; its snapshot moves the tier.
	h.now = effective.Add(time.Minute)
	if err := h.webhook("subscription.updated", h.provider.renew("s1", h.now), nil); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); got != h.proPrice.TierVersionID+"/"+models.AssignmentReasonDowngrade {
		t.Fatalf("assignment after renewal = %s", got)
	}
	if !h.notifier.has("plan_changed") {
		t.Fatal("no plan_changed once the downgrade landed")
	}
	if st, _ := h.svc.PlanState(ctx, tenantID, ""); st.ScheduledChange != nil {
		t.Fatal("applied change still shown")
	}
	tb, _ := h.svc.TenantBilling(ctx, tenantID)
	if len(tb.ScheduledChanges) != 1 || tb.ScheduledChanges[0].Status != models.ScheduledChangeApplied {
		t.Fatalf("changes = %+v", tb.ScheduledChanges)
	}
}

func TestKeepCurrentPlanCancelsDowngrade(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, buyer, effective := downgradeToPro(t, h)

	// Choosing the current plan drops the provider's pending change.
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "max"}); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.cancels) != 1 || h.provider.subs["s1"].PendingProductID != "" || h.mirror("s1").PendingProductID != "" {
		t.Fatalf("cancel = %v, pending %q", h.provider.cancels, h.provider.subs["s1"].PendingProductID)
	}
	if st, _ := h.svc.PlanState(ctx, tenantID, ""); st.ScheduledChange != nil {
		t.Fatal("downgrade still pending")
	}
	// The renewal keeps Max.
	h.now = effective.Add(time.Minute)
	if err := h.webhook("subscription.updated", h.provider.renew("s1", h.now), nil); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); !strings.HasPrefix(got, h.maxPrice.TierVersionID) {
		t.Fatalf("cancelled downgrade still landed: %s", got)
	}
}

func TestKeepCurrentPlanClearsTheProviderEvenWhenTheMirrorMissedIt(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	_, buyer, _ := downgradeToPro(t, h)
	// The mirror lost the pending update (an out-of-order snapshot, say).
	m := h.mirror("s1")
	m.PendingProductID, m.PendingEffectiveAt = "", nil
	if err := h.repo.UpdateSubscription(ctx, nil, m); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: buyer, TierID: "max"}); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.cancels) != 1 || h.provider.subs["s1"].PendingProductID != "" {
		t.Fatalf("the provider still holds the downgrade: cancels %v", h.provider.cancels)
	}
}

func TestKeepCurrentPlanClearsAProviderChangeWithNoLocalRow(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	// The provider holds a downgrade the database never recorded (its write
	// failed after the provider took it); a later snapshot shows it.
	snap := h.sub("s1", maxProduct, models.SubscriptionStatusActive, h.now)
	snap.PendingProductID, snap.PendingEffectiveAt = proProduct, snap.RenewsAt
	h.provider.subs["s1"] = snap
	if err := h.webhook("subscription.created", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"}); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.cancels) != 1 || h.provider.subs["s1"].PendingProductID != "" || h.mirror("s1").PendingProductID != "" {
		t.Fatalf("provider still holds the downgrade: cancels %v", h.provider.cancels)
	}
}

func TestDeclinedUpgradeCancelsTheDroppedDowngrade(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusActive)
	h.now = h.now.Add(time.Minute)
	// A downgrade the provider holds (its target does not matter here).
	m := h.mirror("s1")
	m.PendingProductID = maxProduct
	if err := h.repo.UpdateSubscription(ctx, nil, m); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.InsertScheduledChange(ctx, nil, &models.BillingScheduledChange{ID: "sc1", TenantID: tenantID, BillingSubscriptionID: m.ID,
		FromTierVersionPriceID: h.proPrice.ID, ToTierVersionPriceID: h.proPrice.ID, EffectiveAt: *m.RenewsAt,
		Status: models.ScheduledChangeProviderApplied, CreatedAt: h.now, UpdatedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	h.provider.changeErrs = []error{billingdom.ErrPaymentFailed}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"}); !errors.Is(err, ErrPaymentFailed) {
		t.Fatalf("declined upgrade: %v", err)
	}
	if len(h.provider.cancels) != 1 {
		t.Fatalf("provider cancels = %v", h.provider.cancels)
	}
	tb, _ := h.svc.TenantBilling(ctx, tenantID)
	if len(tb.ScheduledChanges) != 1 || tb.ScheduledChanges[0].Status != models.ScheduledChangeCancelled {
		t.Fatalf("the dropped downgrade is still open: %+v", tb.ScheduledChanges)
	}
}

func TestPricingLeavesArchivedProductsOut(t *testing.T) {
	h := newHarness(t)
	h.provider.mu.Lock()
	p := h.provider.products[maxProduct]
	p.Archived = true
	h.provider.products[maxProduct] = p
	h.provider.mu.Unlock()
	flags, err := h.svc.PricingFlags(t.Context())
	if err != nil || !flags.MappedPrices[h.proPrice.ID] || flags.MappedPrices[h.maxPrice.ID] {
		t.Fatalf("pricing flags = %+v %v", flags, err)
	}
}

func TestUnknownStatusKeepsTheEndOfACancelledSubscription(t *testing.T) {
	h := newHarness(t)
	tenantID, _, _ := h.workspace("a")
	snap := h.sub("s1", proProduct, models.SubscriptionStatusCancelled, h.now)
	ends := h.now.Add(10 * 24 * time.Hour)
	snap.RenewsAt, snap.EndsAt, snap.CancelAtPeriodEnd = nil, &ends, true
	if err := h.webhook("subscription.canceled", snap, map[string]string{"tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	snap.Status, snap.ProviderStatus, snap.EndsAt, snap.UpdatedAt = "", "something_new", nil, h.now.Add(time.Minute)
	if err := h.webhook("subscription.updated", snap, nil); err != nil {
		t.Fatal(err)
	}
	m := h.mirror("s1")
	if m.Status != models.SubscriptionStatusCancelled || m.EndsAt == nil || !m.EndsAt.Equal(ends) {
		t.Fatalf("mirror = %s ends %v", m.Status, m.EndsAt)
	}
	if live, _ := h.svc.HasLiveSubscription(t.Context(), tenantID); !live {
		t.Fatal("the paid period was cut short")
	}
}

func TestCancelAtPeriodEndCancelsDowngrade(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, _, _ := downgradeToPro(t, h)
	snap := h.provider.subs["s1"]
	snap.Status, snap.CancelAtPeriodEnd, snap.EndsAt, snap.UpdatedAt = models.SubscriptionStatusCancelled, true, snap.RenewsAt, h.now.Add(time.Hour)
	if err := h.webhook("subscription.canceled", snap, nil); err != nil {
		t.Fatal(err)
	}
	tb, _ := h.svc.TenantBilling(ctx, tenantID)
	if len(tb.ScheduledChanges) != 1 || tb.ScheduledChanges[0].Status != models.ScheduledChangeCancelled {
		t.Fatalf("changes = %+v", tb.ScheduledChanges)
	}
	if m := h.mirror("s1"); !m.CancelAtPeriodEnd || m.EndsAt == nil {
		t.Fatalf("mirror = %+v", m)
	}
}

func TestReconcileFailsDowngradeTheProviderDropped(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, _, effective := downgradeToPro(t, h)

	// The provider renews on Max: the pending change is gone, the product is
	// still Max.
	h.now = effective.Add(time.Minute)
	snap := h.provider.subs["s1"]
	snap.PendingProductID, snap.PendingEffectiveAt, snap.UpdatedAt = "", nil, h.now
	h.provider.subs["s1"] = snap
	if err := h.webhook("subscription.updated", snap, nil); err != nil {
		t.Fatal(err)
	}
	// Within the grace it is left alone; after it, it fails and the tier stays.
	if err := h.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.svc.PlanState(ctx, tenantID, ""); st.ScheduledChange == nil {
		t.Fatal("failed inside the grace")
	}
	h.now = effective.Add(scheduledChangeGrace + time.Minute)
	if err := h.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, _ := h.svc.Attention(ctx, 50, "")
	if len(items) != 1 || items[0].Reason != AttentionScheduledChangeFailed {
		t.Fatalf("attention = %+v", items)
	}
	if got := h.currentVersion(tenantID); !strings.HasPrefix(got, h.maxPrice.TierVersionID) || !h.notifier.has("plan_change_failed") {
		t.Fatalf("tier after the failure = %s", got)
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
	if err != nil || url != "https://portal/"+tenantID || !exp.After(h.now) || h.provider.portals[0] != tenantID {
		t.Fatalf("portal = %s %v %v", url, exp, err)
	}
}

func TestProviderSwitchCutover(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	lsTenant, lsUser, _ := h.workspace("ls")
	compTenant, _, _ := h.workspace("comp")
	polarTenant, _, _ := h.workspace("polar")
	// A grandfathered Lemon Squeezy workspace, a complimentary one, and one
	// already on Polar.
	if _, err := h.repo.SetBillingRequired(ctx, lsTenant, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.lemonSqueezyRow("ls1", lsTenant, "", false)
	h.lemonSqueezyRow("ls2", compTenant, "", false)
	if err := h.svc.SetComplimentary(ctx, compTenant, true, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := h.webhook("subscription.created", h.sub("s1", proProduct, models.SubscriptionStatusActive, h.now), map[string]string{"tenant_id": polarTenant}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.InvalidateSubscriptions(ctx, true, 0); !errors.Is(err, ErrBadScope) {
		t.Fatalf("no scope: %v", err)
	}
	ids, err := h.svc.InvalidateSubscriptions(ctx, true, ScopeProviderSwitch)
	if err != nil || len(ids) != 2 || !strings.Contains(strings.Join(ids, ","), lsTenant) || !strings.Contains(strings.Join(ids, ","), compTenant) {
		t.Fatalf("dry run = %v %v", ids, err)
	}
	if g, _ := h.svc.Gated(ctx, lsTenant); g {
		t.Fatal("dry run re-gated")
	}
	if _, err := h.svc.InvalidateSubscriptions(ctx, false, ScopeProviderSwitch); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, lsTenant); !g {
		t.Fatal("the grandfathered Lemon Squeezy workspace was not re-gated")
	}
	if g, _ := h.svc.Gated(ctx, compTenant); g {
		t.Fatal("a complimentary workspace was gated")
	}
	if g, _ := h.svc.Gated(ctx, polarTenant); g {
		t.Fatal("a Polar workspace was touched")
	}
	sent := map[string]bool{}
	for _, e := range h.jobs.emails {
		if e.key == "billing_reentry_required" {
			sent[e.userID] = true
		}
	}
	if !sent[lsUser] || len(sent) != 2 {
		t.Fatalf("emails = %+v", h.jobs.emails)
	}
	tb, _ := h.svc.TenantBilling(ctx, lsTenant)
	if len(tb.Subscriptions) != 1 || tb.Subscriptions[0].InvalidatedReason != models.InvalidatedReasonProviderSwitch {
		t.Fatalf("ls subscription = %+v", tb.Subscriptions)
	}
	if !h.notifier.has("reentry_required") {
		t.Fatal("no reentry notice")
	}
	if ids, _ := h.svc.InvalidateSubscriptions(ctx, true, ScopeProviderSwitch); len(ids) != 0 {
		t.Fatalf("second run still finds %v", ids)
	}
}

func TestModeCutover(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusActive)
	if _, err := h.svc.InvalidateSubscriptions(ctx, true, ScopeModeCutover); !errors.Is(err, ErrStillTestMode) {
		t.Fatalf("cutover in test mode: %v", err)
	}
	h.svc.d.Settings.TestMode = false
	ids, err := h.svc.InvalidateSubscriptions(ctx, true, ScopeModeCutover)
	if err != nil || len(ids) != 1 || ids[0] != tenantID {
		t.Fatalf("dry run = %v %v", ids, err)
	}
	if _, err := h.svc.InvalidateSubscriptions(ctx, false, ScopeModeCutover); err != nil {
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
	if ids, _ := h.svc.InvalidateSubscriptions(ctx, true, ScopeModeCutover); len(ids) != 0 {
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
