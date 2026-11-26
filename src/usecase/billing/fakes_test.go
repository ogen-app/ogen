package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
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

// fakeProvider is an in-memory provider with Polar's plan-change semantics:
// a next-period change is held as pending until renew applies it. Webhooks
// are the JSON of a billingdom.Event and verify when the signature is "ok".
type fakeProvider struct {
	mu        sync.Mutex
	subs      map[string]billingdom.Subscription
	products  map[string]billingdom.Product
	checkouts []billingdom.CheckoutRequest
	changes   []fakeChange
	cancels   []string
	portals   []string
	changeErr error
	// changeErrs fail the next ChangePlan calls in order, before changeErr.
	changeErrs []error
	// catalogErr fails Catalog; liveKey makes live mode readable.
	catalogErr error
	liveKey    bool
	clock      func() time.Time
	// seq makes every provider-side update newer than the last.
	seq int
	// duringChange runs once, at the start of the next ChangePlan and outside
	// the lock, to interleave a caller with a provider call in flight.
	duringChange func()
}

type fakeChange struct {
	subID, productID string
	opts             billingdom.ChangeOpts
}

func newFakeProvider(clock func() time.Time) *fakeProvider {
	return &fakeProvider{subs: map[string]billingdom.Subscription{}, products: map[string]billingdom.Product{}, clock: clock}
}

func (f *fakeProvider) CreateCheckout(_ context.Context, req billingdom.CheckoutRequest) (billingdom.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkouts = append(f.checkouts, req)
	return billingdom.CheckoutSession{URL: "https://checkout/" + strconv.Itoa(len(f.checkouts)), ExpiresAt: f.clock().Add(time.Hour)}, nil
}

func (f *fakeProvider) GetSubscription(_ context.Context, id string) (billingdom.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[id]
	if !ok {
		return billingdom.Subscription{}, billingdom.ErrNotFound
	}
	return s, nil
}

func (f *fakeProvider) ChangePlan(_ context.Context, id, productID string, opts billingdom.ChangeOpts) (billingdom.Subscription, error) {
	f.mu.Lock()
	hook := f.duringChange
	f.duringChange = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.changeErrs) > 0 {
		err := f.changeErrs[0]
		f.changeErrs = f.changeErrs[1:]
		return billingdom.Subscription{}, err
	}
	if f.changeErr != nil {
		return billingdom.Subscription{}, f.changeErr
	}
	f.changes = append(f.changes, fakeChange{id, productID, opts})
	s := f.subs[id]
	if opts.Timing == billingdom.NextPeriod {
		s.PendingProductID, s.PendingEffectiveAt = productID, s.RenewsAt
		if productID == s.ProductID {
			s.PendingProductID, s.PendingEffectiveAt = "", nil
		}
	} else {
		s.ProductID, s.PendingProductID, s.PendingEffectiveAt = productID, "", nil
	}
	if opts.EndTrialNow {
		s.Status, s.TrialEndsAt = models.SubscriptionStatusActive, nil
	}
	f.seq++
	s.UpdatedAt = f.clock().Add(time.Duration(f.seq) * time.Second)
	f.subs[id] = s
	return s, nil
}

func (f *fakeProvider) CancelScheduledChange(_ context.Context, id string) (billingdom.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changeErr != nil {
		return billingdom.Subscription{}, f.changeErr
	}
	f.cancels = append(f.cancels, id)
	s := f.subs[id]
	s.PendingProductID, s.PendingEffectiveAt = "", nil
	f.seq++
	s.UpdatedAt = f.clock().Add(time.Duration(f.seq) * time.Second)
	f.subs[id] = s
	return s, nil
}

// renew applies a subscription's pending change, as the provider does at
// renewal, and returns the new snapshot.
func (f *fakeProvider) renew(id string, at time.Time) billingdom.Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[id]
	if s.PendingProductID != "" {
		s.ProductID = s.PendingProductID
	}
	s.PendingProductID, s.PendingEffectiveAt = "", nil
	next := at.Add(30 * 24 * time.Hour)
	s.RenewsAt, s.UpdatedAt = &next, at
	f.subs[id] = s
	return s
}

func (f *fakeProvider) PortalSession(_ context.Context, customerExternalID string) (billingdom.PortalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.portals = append(f.portals, customerExternalID)
	return billingdom.PortalSession{URL: "https://portal/" + customerExternalID, ExpiresAt: f.clock().Add(time.Hour)}, nil
}

func (f *fakeProvider) GetProduct(_ context.Context, id string) (billingdom.Product, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.products[id]
	if !ok {
		return billingdom.Product{}, billingdom.ErrNotFound
	}
	return p, nil
}

// Catalog lists the fake's products of the asked mode; liveKey false makes
// production unreadable, like a missing polar_access_token_live.
func (f *fakeProvider) Catalog(_ context.Context, testMode bool) (billingdom.Catalog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.catalogErr != nil {
		return billingdom.Catalog{}, f.catalogErr
	}
	if !testMode && !f.liveKey {
		return billingdom.Catalog{}, billingdom.ErrNotConfigured
	}
	out := billingdom.Catalog{TestMode: testMode}
	for _, p := range f.products {
		if p.TestMode == testMode {
			out.Products = append(out.Products, p)
		}
	}
	slices.SortFunc(out.Products, func(a, b billingdom.Product) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *fakeProvider) VerifyWebhook(_ context.Context, h billingdom.WebhookHeaders, _ []byte) error {
	if h.Signature != "ok" {
		return billingdom.ErrInvalidSignature
	}
	return nil
}

func (f *fakeProvider) ParseWebhook(h billingdom.WebhookHeaders, body []byte) (billingdom.Event, error) {
	var ev billingdom.Event
	err := json.Unmarshal(body, &ev)
	ev.ID = h.ID
	return ev, err
}

func (f *fakeProvider) PaymentSources() []string {
	return []string{billingdom.PaymentSourceOrder, billingdom.PaymentSourceRefund}
}

func (f *fakeProvider) ListPayments(context.Context, string, int) (billingdom.PaymentPage, error) {
	return billingdom.PaymentPage{LastPage: 1}, nil
}

type sentEmail struct{ userID, tenantID, key, idem string }

type fakeJobs struct {
	mu        sync.Mutex
	emails    []sentEmail
	teardowns []string
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
// published at monthly EUR prices mapped to products pro-p / max-p.
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
	events   int
}

const (
	proProduct = "prod_pro"
	maxProduct = "prod_max"
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

	settings := billingdom.Settings{Provider: models.BillingProviderPolar, TestMode: true, OrganizationID: "org", APIVersion: "2026-10", AppBaseURL: "https://app"}
	for _, o := range opts {
		o(&settings)
	}
	h.provider.products[proProduct] = billingdom.Product{ID: proProduct, Name: "Pro monthly", Interval: "month", IntervalCount: 1,
		Prices: []billingdom.ProductPrice{{Currency: "EUR", AmountMinor: 2900}}, HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1, TestMode: true}
	h.provider.products[maxProduct] = billingdom.Product{ID: maxProduct, Name: "Max monthly", Interval: "month", IntervalCount: 1,
		Prices: []billingdom.ProductPrice{{Currency: "EUR", AmountMinor: 7900}}, TestMode: true}

	h.repo = repository.NewBillingRepository(db)
	for priceID, productID := range map[string]string{proID: proProduct, maxID: maxProduct} {
		if err := h.repo.UpsertProductMapping(t.Context(), &models.BillingProductMapping{
			TierVersionPriceID: priceID, TestMode: settings.TestMode, ProductID: productID, Currency: "EUR", UpdatedAt: h.now, UpdatedBy: models.ProductMappingByHarbor,
		}); err != nil {
			t.Fatal(err)
		}
	}
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
	return h.publishPrice(versions, tierID, "test", "month", net, true)
}

// publishPrice creates a purchasable version of a seeded tier with one EUR
// price at the interval, published when publish is set, and returns the
// price's id.
func (h *harness) publishPrice(versions repository.TenantTierVersionRepository, tierID, suffix, interval string, net int64, publish bool) string {
	ctx := h.t.Context()
	n, err := versions.NextVersion(ctx, tierID)
	if err != nil {
		h.t.Fatal(err)
	}
	vid, pid := "ttv-"+tierID+"-"+suffix, "price-"+tierID+"-"+interval[:1]
	if suffix != "test" {
		pid += "-" + suffix
	}
	v := &models.TenantTierVersion{ID: vid, TierID: tierID, Version: n, Status: models.TierVersionStatusDraft, Purchasable: true, Entitlements: map[string]any{}}
	if err := versions.Create(ctx, v, []models.TenantTierVersionPrice{{ID: pid, TierVersionID: vid, Currency: "EUR", BillingInterval: interval, NetMinor: net}}); err != nil {
		h.t.Fatal(err)
	}
	if !publish {
		return pid
	}
	if _, err := versions.Publish(ctx, vid, "test", h.supersedeOnSale(versions, tierID), h.now.Add(-time.Hour)); err != nil {
		h.t.Fatal(err)
	}
	return pid
}

// supersedeOnSale retires the tier's active purchasable version, if any, as
// the next one is published, grandfathering its tenants: a tier sells one
// version at a time.
func (h *harness) supersedeOnSale(versions repository.TenantTierVersionRepository, tierID string) repository.SupersedeOptions {
	all, err := versions.ListByTier(h.t.Context(), tierID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range all {
		if v.Status == models.TierVersionStatusActive && v.Purchasable {
			return repository.SupersedeOptions{VersionID: v.ID, Force: true}
		}
	}
	return repository.SupersedeOptions{}
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
func (h *harness) sub(id, product, status string, updated time.Time) billingdom.Subscription {
	s := billingdom.Subscription{ID: id, CustomerID: "cus-" + id, ProductID: product, Currency: "EUR", Status: status, ProviderStatus: status,
		TestMode: true, UpdatedAt: updated, CardBrand: "visa", CardLastFour: "4242"}
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

// webhook delivers a subscription snapshot. metadata is the checkout's,
// copied onto the snapshot as the provider does; each delivery gets a fresh
// event id.
func (h *harness) webhook(name string, s billingdom.Subscription, metadata map[string]string) error {
	if metadata != nil {
		s.Metadata = metadata
	}
	ev := billingdom.Event{Name: name, Kind: billingdom.EventSubscription, TestMode: s.TestMode, ObjectID: s.ID, Metadata: s.Metadata, Subscription: &s}
	return h.deliver(ev)
}

// deliver sends an event as a webhook with a fresh event id.
func (h *harness) deliver(ev billingdom.Event) error {
	h.events++
	return h.deliverAs("evt_"+strconv.Itoa(h.events), ev)
}

// deliverAs sends an event as a webhook with the given event id.
func (h *harness) deliverAs(eventID string, ev billingdom.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.svc.HandleWebhook(h.t.Context(), billingdom.WebhookHeaders{ID: eventID, Timestamp: "0", Signature: "ok"}, body)
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
	s, err := h.repo.GetSubscriptionByProviderID(h.t.Context(), nil, models.BillingProviderPolar, providerID, false)
	if err != nil {
		h.t.Fatalf("mirror %s: %v", providerID, err)
	}
	return s
}
