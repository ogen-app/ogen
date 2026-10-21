package repository_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

func newBillingTenant(t *testing.T, db *bun.DB, id string, required bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.NewInsert().Model(&models.Tenant{ID: id, Name: "WS " + id, Slug: id, TierID: models.DefaultTierID, BillingRequired: required, CreatedAt: now, UpdatedAt: now}).Exec(t.Context()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func newBillingAccount(t *testing.T, db *bun.DB, id string) {
	t.Helper()
	if _, err := db.NewInsert().Model(&models.Account{ID: id, Email: id + "@example.com", Name: id}).Exec(t.Context()); err != nil {
		t.Fatalf("insert account: %v", err)
	}
}

func newSub(tenantID, providerID string) *models.BillingSubscription {
	return &models.BillingSubscription{
		ID: "bs-" + providerID, TenantID: tenantID, Provider: models.BillingProviderLemonSqueezy,
		ProviderSubscriptionID: providerID, Status: models.SubscriptionStatusActive, TestMode: true,
		ProviderUpdatedAt: time.Now().UTC(),
	}
}

func TestBillingGateAndSubscriptions(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	repo := repository.NewBillingRepository(db)
	newBillingTenant(t, db, "t-gated", true)
	newBillingTenant(t, db, "t-free", false)

	if g, err := repo.Gated(ctx, "t-gated"); err != nil || !g {
		t.Fatalf("gated tenant: %v %v", g, err)
	}
	if g, _ := repo.Gated(ctx, "t-free"); g {
		t.Fatal("grandfathered tenant gated")
	}
	if g, err := repo.Gated(ctx, "missing"); err != nil || g {
		t.Fatalf("missing tenant: %v %v", g, err)
	}

	sub := newSub("t-gated", "9")
	if err := repo.InsertSubscription(ctx, nil, sub); err != nil {
		t.Fatal(err)
	}
	if g, _ := repo.Gated(ctx, "t-gated"); g {
		t.Fatal("still gated after a subscription")
	}
	// An expired subscription still lifts the gate; only invalidation re-gates.
	sub.Status = models.SubscriptionStatusExpired
	if err := repo.UpdateSubscription(ctx, nil, sub); err != nil {
		t.Fatal(err)
	}
	if g, _ := repo.Gated(ctx, "t-gated"); g {
		t.Fatal("expired subscription re-gated")
	}
	if err := repo.Invalidate(ctx, nil, []string{sub.ID}, models.InvalidatedReasonModeCutover, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g, _ := repo.Gated(ctx, "t-gated"); !g {
		t.Fatal("invalidated subscription did not re-gate")
	}

	got, err := repo.GetSubscriptionByProviderID(ctx, nil, models.BillingProviderLemonSqueezy, "9", false)
	if err != nil || got.InvalidatedReason != models.InvalidatedReasonModeCutover || got.Status != models.SubscriptionStatusExpired {
		t.Fatalf("by provider id: %+v %v", got, err)
	}
	if _, err := repo.GetSubscriptionByProviderID(ctx, nil, models.BillingProviderLemonSqueezy, "nope", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing sub err = %v", err)
	}
	if err := repo.InsertSubscription(ctx, nil, newSub("t-free", "9")); err == nil {
		t.Fatal("duplicate provider subscription id accepted")
	}

	if ok, err := repo.SetBillingRequired(ctx, "t-free", true, time.Now()); err != nil || !ok {
		t.Fatalf("set billing required: %v %v", ok, err)
	}
	if g, _ := repo.Gated(ctx, "t-free"); !g {
		t.Fatal("billing_required not applied")
	}
}

func TestBillingTrialUsedAndReconcileListing(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	repo := repository.NewBillingRepository(db)
	newBillingTenant(t, db, "t1", true)
	newBillingAccount(t, db, "acc-1")

	trial := newSub("t1", "1")
	acc := "acc-1"
	end := time.Now().Add(30 * 24 * time.Hour)
	trial.AccountID, trial.TrialEndsAt, trial.Status = &acc, &end, models.SubscriptionStatusOnTrial
	if err := repo.InsertSubscription(ctx, nil, trial); err != nil {
		t.Fatal(err)
	}
	if used, _ := repo.TrialUsed(ctx, "acc-1", true); !used {
		t.Fatal("trial not counted in its mode")
	}
	if used, _ := repo.TrialUsed(ctx, "acc-1", false); used {
		t.Fatal("test-mode trial counted in live mode")
	}

	expired := newSub("t1", "2")
	expired.Status = models.SubscriptionStatusExpired
	if err := repo.InsertSubscription(ctx, nil, expired); err != nil {
		t.Fatal(err)
	}
	list, err := repo.ListForReconcile(ctx, true, "", 10)
	if err != nil || len(list) != 1 || list[0].ID != trial.ID {
		t.Fatalf("reconcile list = %+v %v", list, err)
	}
	// The workspace's live trial still entitles it: nothing to tear down.
	if unpaid, err := repo.ListUnpaidExpired(ctx, true, 10); err != nil || len(unpaid) != 0 {
		t.Fatalf("unpaid expired with a live subscription = %+v %v", unpaid, err)
	}
	trial.Status = models.SubscriptionStatusExpired
	if err := repo.UpdateSubscription(ctx, nil, trial); err != nil {
		t.Fatal(err)
	}
	unpaid, err := repo.ListUnpaidExpired(ctx, true, 10)
	if err != nil || len(unpaid) != 2 {
		t.Fatalf("unpaid expired = %+v %v", unpaid, err)
	}
	live, err := repo.ListTestModeLive(ctx)
	if err != nil || len(live) != 2 {
		t.Fatalf("test-mode live = %d %v", len(live), err)
	}
}

func TestBillingWebhookEvents(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	repo := repository.NewBillingRepository(db)
	newBillingTenant(t, db, "t1", false)

	ev := &models.BillingWebhookEvent{ID: "we-1", Provider: models.BillingProviderLemonSqueezy, EventName: "subscription_created", BodySHA256: "abc", Payload: json.RawMessage(`{}`)}
	if _, inserted, err := repo.InsertWebhookEvent(ctx, ev); err != nil || !inserted {
		t.Fatalf("first insert: %v %v", inserted, err)
	}
	dup := &models.BillingWebhookEvent{ID: "we-2", Provider: models.BillingProviderLemonSqueezy, EventName: "subscription_created", BodySHA256: "abc", Payload: json.RawMessage(`{}`)}
	existing, inserted, err := repo.InsertWebhookEvent(ctx, dup)
	if err != nil || inserted || existing == nil || existing.ID != "we-1" {
		t.Fatalf("duplicate insert: %+v %v %v", existing, inserted, err)
	}
	tid := "t1"
	if err := repo.FinishWebhookEvent(ctx, "we-1", models.WebhookOutcomeFailed, "boom", &tid, time.Now()); err != nil {
		t.Fatal(err)
	}
	evs, err := repo.ListWebhookEventsByTenant(ctx, "t1", 20)
	if err != nil || len(evs) != 1 || evs[0].Outcome != models.WebhookOutcomeFailed {
		t.Fatalf("by tenant = %+v %v", evs, err)
	}
	if failed, _ := repo.ListFailedWebhookEvents(ctx, time.Now().Add(-time.Hour), true); len(failed) != 0 {
		t.Fatal("resolved failure listed as unresolved")
	}
	if n, err := repo.DeleteWebhookEventsBefore(ctx, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("delete = %d %v", n, err)
	}
}

func TestBillingFreeTrialTierAndPrices(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	repo := repository.NewBillingRepository(db)

	if tier, err := repo.FreeTrialTier(ctx); err != nil || tier != nil {
		t.Fatalf("offer on by default: %+v %v", tier, err)
	}
	if ok, err := repo.SetFreeTrialTier(ctx, "pro", time.Now()); err != nil || !ok {
		t.Fatalf("mark pro: %v %v", ok, err)
	}
	if ok, err := repo.SetFreeTrialTier(ctx, "max", time.Now()); err != nil || !ok {
		t.Fatalf("mark max: %v %v", ok, err)
	}
	if tier, _ := repo.FreeTrialTier(ctx); tier == nil || tier.ID != "max" {
		t.Fatalf("marked tier = %+v", tier)
	}
	if ok, err := repo.SetFreeTrialTier(ctx, "nope", time.Now()); err != nil || ok {
		t.Fatalf("unknown tier: %v %v", ok, err)
	}
	if tier, _ := repo.FreeTrialTier(ctx); tier == nil || tier.ID != "max" {
		t.Fatal("unknown tier cleared the previous mark")
	}
	if ok, err := repo.SetFreeTrialTier(ctx, "", time.Now()); err != nil || !ok {
		t.Fatalf("clear: %v %v", ok, err)
	}
	if tier, _ := repo.FreeTrialTier(ctx); tier != nil {
		t.Fatal("offer still on after clearing")
	}

	vr := repository.NewTenantTierVersionRepository(db)
	prices, err := vr.PricesByVersion(ctx, "ttv-trial-v1")
	if err != nil || len(prices) == 0 {
		t.Fatalf("seeded prices: %v %v", prices, err)
	}
	got, err := repo.Prices(ctx, []string{prices[0].ID, "missing"})
	if err != nil || len(got) != 1 {
		t.Fatalf("prices = %+v %v", got, err)
	}
	p := got[prices[0].ID]
	if p.TierID != "trial" || p.TierVersionID != "ttv-trial-v1" || p.TierName == "" || p.Interval != "month" {
		t.Fatalf("price = %+v", p)
	}
}

func TestBillingScheduledChanges(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	repo := repository.NewBillingRepository(db)
	newBillingTenant(t, db, "t1", false)
	sub := newSub("t1", "1")
	if err := repo.InsertSubscription(ctx, nil, sub); err != nil {
		t.Fatal(err)
	}
	prices, _ := repository.NewTenantTierVersionRepository(db).PricesByVersion(ctx, "ttv-trial-v1")
	now := time.Now().UTC()
	c := &models.BillingScheduledChange{ID: "sc-1", TenantID: "t1", BillingSubscriptionID: sub.ID, FromTierVersionPriceID: prices[0].ID, ToTierVersionPriceID: prices[0].ID, EffectiveAt: now.Add(30 * time.Minute), Status: models.ScheduledChangePending}
	if err := repo.InsertScheduledChange(ctx, nil, c); err != nil {
		t.Fatal(err)
	}
	second := *c
	second.ID = "sc-2"
	if err := repo.InsertScheduledChange(ctx, nil, &second); err == nil {
		t.Fatal("two open changes on one subscription accepted")
	}
	due, err := repo.ListDueScheduledChanges(ctx, now, time.Hour)
	if err != nil || len(due) != 1 {
		t.Fatalf("due (pending within lead) = %+v %v", due, err)
	}
	if moved, _ := repo.SetScheduledChangeStatus(ctx, nil, "sc-1", []string{models.ScheduledChangePending}, models.ScheduledChangeProviderApplied, "", now); !moved {
		t.Fatal("pending → provider_applied did not move")
	}
	if due, _ := repo.ListDueScheduledChanges(ctx, now, time.Hour); len(due) != 0 {
		t.Fatal("provider_applied change due before effective_at")
	}
	if due, _ := repo.ListDueScheduledChanges(ctx, now.Add(time.Hour), time.Hour); len(due) != 1 {
		t.Fatal("provider_applied change not due after effective_at")
	}
	if moved, _ := repo.SetScheduledChangeStatus(ctx, nil, "sc-1", []string{models.ScheduledChangePending}, models.ScheduledChangeCancelled, "", now); moved {
		t.Fatal("status guard ignored")
	}
	open, err := repo.OpenScheduledChange(ctx, nil, sub.ID)
	if err != nil || open.ID != "sc-1" {
		t.Fatalf("open change = %+v %v", open, err)
	}
}

func TestGetForAuthReportsBillingGate(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	newBillingTenant(t, db, "t-gated", true)
	newBillingAccount(t, db, "acc-1")
	if _, err := db.NewInsert().Model(&models.User{ID: "u-1", AccountID: "acc-1", TenantID: "t-gated", Name: "u", Email: "acc-1@example.com", Role: models.RoleOwner}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	sessions := repository.NewSessionRepository(db)
	if err := sessions.Create(ctx, &models.Session{ID: "sess-1", AccountID: "acc-1", UserID: "u-1", TenantID: "t-gated", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, m, err := sessions.GetForAuth(ctx, "sess-1", "")
	if err != nil || m == nil || !m.BillingGated {
		t.Fatalf("gated membership = %+v %v", m, err)
	}
	if err := repository.NewBillingRepository(db).InsertSubscription(ctx, nil, newSub("t-gated", "9")); err != nil {
		t.Fatal(err)
	}
	if _, m, _ = sessions.GetForAuth(ctx, "sess-1", ""); m == nil || m.BillingGated {
		t.Fatalf("membership after checkout = %+v", m)
	}
}
