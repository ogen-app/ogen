package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// fakeProvider is an in-memory provider. Webhooks are the JSON of a
// billingdom.Event and verify when the signature is "ok".
type fakeProvider struct {
	mu        sync.Mutex
	subs      map[string]billingdom.Subscription
	variants  map[string]billingdom.Variant
	checkouts []billingdom.CheckoutRequest
	changes   []fakeChange
	changeErr error
	clock     func() time.Time
	// duringChange runs once, at the start of the next ChangePlan and outside
	// the lock, to interleave a caller with a provider call in flight.
	duringChange func()
}

type fakeChange struct {
	subID, variantID string
	opts             billingdom.ChangeOpts
}

func newFakeProvider(clock func() time.Time) *fakeProvider {
	return &fakeProvider{subs: map[string]billingdom.Subscription{}, variants: map[string]billingdom.Variant{}, clock: clock}
}

func (f *fakeProvider) CreateCheckout(_ context.Context, req billingdom.CheckoutRequest) (billingdom.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkouts = append(f.checkouts, req)
	return billingdom.CheckoutSession{URL: "https://checkout/" + strconv.Itoa(len(f.checkouts)), ExpiresAt: req.ExpiresAt}, nil
}

func (f *fakeProvider) GetSubscription(_ context.Context, id string) (billingdom.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[id]
	if !ok {
		return billingdom.Subscription{}, billingdom.ErrNotFound
	}
	s.PortalURL = "https://portal/" + id
	return s, nil
}

func (f *fakeProvider) ChangePlan(_ context.Context, id, variantID string, opts billingdom.ChangeOpts) (billingdom.Subscription, error) {
	f.mu.Lock()
	hook := f.duringChange
	f.duringChange = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changeErr != nil {
		return billingdom.Subscription{}, f.changeErr
	}
	f.changes = append(f.changes, fakeChange{id, variantID, opts})
	s := f.subs[id]
	s.VariantID = variantID
	if opts.EndTrialNow {
		s.Status, s.TrialEndsAt = models.SubscriptionStatusActive, nil
	}
	s.UpdatedAt = f.clock().Add(time.Duration(len(f.changes)) * time.Second)
	f.subs[id] = s
	return s, nil
}

func (f *fakeProvider) GetVariant(_ context.Context, id string) (billingdom.Variant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.variants[id]
	if !ok {
		return billingdom.Variant{}, billingdom.ErrNotFound
	}
	return v, nil
}

func (f *fakeProvider) VerifyWebhook(_ context.Context, sig string, _ []byte) error {
	if sig != "ok" {
		return billingdom.ErrInvalidSignature
	}
	return nil
}

func (f *fakeProvider) ParseWebhook(body []byte) (billingdom.Event, error) {
	var ev billingdom.Event
	err := json.Unmarshal(body, &ev)
	return ev, err
}

type sentEmail struct{ userID, tenantID, key, idem string }

type fakeJobs struct {
	mu        sync.Mutex
	emails    []sentEmail
	teardowns []string
	steps     []string
}

func (j *fakeJobs) EnqueueBillingEmailTx(_ context.Context, _ *sql.Tx, userID, tenantID, key, idem string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.emails = append(j.emails, sentEmail{userID, tenantID, key, idem})
	return nil
}

func (j *fakeJobs) EnqueueTrialTeardownTx(_ context.Context, _ *sql.Tx, tenantID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.teardowns = append(j.teardowns, tenantID)
	return nil
}

func (j *fakeJobs) EnqueueScheduledChangeStepTx(_ context.Context, _ *sql.Tx, changeID string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.steps = append(j.steps, changeID+"@"+at.UTC().Format(time.RFC3339))
	return nil
}

type fakeNotifier struct {
	mu    sync.Mutex
	types []string
}

func (n *fakeNotifier) EmitToUsers(_ context.Context, _ []string, spec notify.Spec) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.types = append(n.types, spec.Type)
	return nil
}

func (n *fakeNotifier) has(typ string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.types, "billing."+typ)
}

// harness is a billing service over a fresh database with Pro and Max
// published at monthly prices mapped to variants pro-v / max-v.
type harness struct {
	t        *testing.T
	db       *bun.DB
	svc      *Service
	repo     repository.BillingRepository
	provider *fakeProvider
	jobs     *fakeJobs
	notifier *fakeNotifier
	now      time.Time
	proPrice billingdom.Price
	maxPrice billingdom.Price
}

const (
	proVariant = "101"
	maxVariant = "202"
)

func newHarness(t *testing.T, opts ...func(*billingdom.Settings)) *harness {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	h := &harness{t: t, db: db, now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), jobs: &fakeJobs{}, notifier: &fakeNotifier{}}
	clock := func() time.Time { return h.now }
	h.provider = newFakeProvider(clock)
	versions := repository.NewTenantTierVersionRepository(db)
	proID := h.publishVersion(versions, "pro", 2900)
	maxID := h.publishVersion(versions, "max", 7900)

	settings := billingdom.Settings{Provider: models.BillingProviderLemonSqueezy, TestMode: true, StoreID: "1", AppBaseURL: "https://app"}
	variants, err := billingdom.ParseVariantMap(fmt.Sprintf(`{%q:%q,%q:%q}`, proID, proVariant, maxID, maxVariant))
	if err != nil {
		t.Fatal(err)
	}
	settings.Variants = variants
	for _, o := range opts {
		o(&settings)
	}
	h.provider.variants[proVariant] = billingdom.Variant{ID: proVariant, PriceMinor: 2900, Interval: "month", HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1, TestMode: true}
	h.provider.variants[maxVariant] = billingdom.Variant{ID: maxVariant, PriceMinor: 7900, Interval: "month", TestMode: true}

	h.repo = repository.NewBillingRepository(db)
	h.svc = New(Deps{
		DB: db, Repo: h.repo,
		Tenants: repository.NewTenantRepository(db), Users: repository.NewUserRepository(db),
		Tiers: repository.NewTenantTierRepository(db), TierVersions: versions,
		Assignments: repository.NewTenantTierAssignmentRepository(db),
		Provider:    h.provider, Settings: settings, Notifier: h.notifier, Jobs: h.jobs, Now: clock,
	})
	prices, err := h.repo.Prices(t.Context(), []string{proID, maxID})
	if err != nil {
		t.Fatal(err)
	}
	h.proPrice, h.maxPrice = prices[proID], prices[maxID]
	return h
}

// publishVersion publishes a new purchasable version of a seeded tier with
// one monthly EUR price and returns that price's id.
func (h *harness) publishVersion(versions repository.TenantTierVersionRepository, tierID string, net int64) string {
	ctx := h.t.Context()
	n, err := versions.NextVersion(ctx, tierID)
	if err != nil {
		h.t.Fatal(err)
	}
	vid, pid := "ttv-"+tierID+"-test", "price-"+tierID+"-m"
	v := &models.TenantTierVersion{ID: vid, TierID: tierID, Version: n, Status: models.TierVersionStatusDraft, Purchasable: true, Entitlements: map[string]any{}}
	if err := versions.Create(ctx, v, []models.TenantTierVersionPrice{{ID: pid, TierVersionID: vid, Currency: "EUR", BillingInterval: "month", NetMinor: net}}); err != nil {
		h.t.Fatal(err)
	}
	if err := versions.Publish(ctx, vid, "test", h.now.Add(-time.Hour)); err != nil {
		h.t.Fatal(err)
	}
	return pid
}

// workspace creates a gated tenant with one owner and returns ids.
func (h *harness) workspace(id string) (tenantID, userID, accountID string) {
	h.t.Helper()
	ctx := h.t.Context()
	tenantID, userID, accountID = "t-"+id, "u-"+id, "a-"+id
	if _, err := h.db.NewInsert().Model(&models.Tenant{ID: tenantID, Name: "WS " + id, Slug: tenantID, TierID: models.DefaultTierID, BillingRequired: true, CreatedAt: h.now, UpdatedAt: h.now}).Exec(ctx); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.db.NewInsert().Model(&models.Account{ID: accountID, Email: id + "@example.com", Name: id}).Exec(ctx); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.db.NewInsert().Model(&models.User{ID: userID, AccountID: accountID, TenantID: tenantID, Name: id, Email: id + "@example.com", Role: models.RoleOwner}).Exec(ctx); err != nil {
		h.t.Fatal(err)
	}
	return
}

// sub stores a provider-side subscription and returns its snapshot.
func (h *harness) sub(id, variant, status string, updated time.Time) billingdom.Subscription {
	s := billingdom.Subscription{ID: id, VariantID: variant, Status: status, TestMode: true, UpdatedAt: updated, CardBrand: "visa", CardLastFour: "4242"}
	renews := h.now.Add(30 * 24 * time.Hour)
	s.RenewsAt = &renews
	if status == models.SubscriptionStatusOnTrial {
		s.TrialEndsAt = &renews
	}
	h.provider.mu.Lock()
	h.provider.subs[id] = s
	h.provider.mu.Unlock()
	return s
}

func (h *harness) webhook(name string, s billingdom.Subscription, custom map[string]string) error {
	ev := billingdom.Event{Name: name, Kind: billingdom.EventSubscription, TestMode: s.TestMode, ObjectID: s.ID, CustomData: custom, Subscription: &s}
	body, err := json.Marshal(ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.svc.HandleWebhook(h.t.Context(), "ok", body)
}

func (h *harness) currentVersion(tenantID string) string {
	h.t.Helper()
	a, err := repository.NewTenantTierAssignmentRepository(h.db).CoveringAt(h.t.Context(), tenantID, h.now.Add(time.Second))
	if err != nil {
		return ""
	}
	return a.TierVersionID + "/" + a.Reason
}

func (h *harness) mirror(providerID string) *models.BillingSubscription {
	h.t.Helper()
	s, err := h.repo.GetSubscriptionByProviderID(h.t.Context(), nil, models.BillingProviderLemonSqueezy, providerID, false)
	if err != nil {
		h.t.Fatalf("mirror %s: %v", providerID, err)
	}
	return s
}
