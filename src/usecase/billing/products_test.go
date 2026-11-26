package billing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type fakeLister struct {
	mu    sync.Mutex
	rows  []models.BillingProductMapping
	err   error
	calls int
}

func (f *fakeLister) ListProductMappings(context.Context, bool) ([]models.BillingProductMapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rows, f.err
}

// product builds a recurring EUR product; trial adds a one-month trial.
func product(id, name, interval string, amount int64, trial, testMode bool) billingdom.Product {
	p := billingdom.Product{ID: id, Name: name, Interval: interval, IntervalCount: 1, Prices: []billingdom.ProductPrice{{Currency: "EUR", AmountMinor: amount}}, TestMode: testMode}
	if trial {
		p.HasFreeTrial, p.TrialInterval, p.TrialIntervalCount = true, "month", 1
	}
	return p
}

func mapped(m billingdom.ProductMap, priceID string) string {
	id, _ := m.Product(priceID)
	return id
}

func TestProductStoreCachesAndKeepsLastGoodMap(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	row := func(product string) []models.BillingProductMapping {
		return []models.BillingProductMapping{{TierVersionPriceID: "p1", ProductID: product, Currency: "EUR"}}
	}
	lister := &fakeLister{rows: row("prod_1")}
	store := NewProductStore(lister, true)
	store.now = func() time.Time { return now }

	if _, err := NewProductStore(&fakeLister{err: errors.New("down")}, true).Current(ctx); err == nil {
		t.Fatal("a store that never loaded served a map")
	}
	m, err := store.Current(ctx)
	if err != nil || mapped(m, "p1") != "prod_1" {
		t.Fatalf("first load: %v %v", m, err)
	}

	// Within the TTL a change in the table is not seen; after it, it is.
	lister.rows = row("prod_2")
	now = now.Add(productMapTTL - time.Second)
	if m, _ := store.Current(ctx); mapped(m, "p1") != "prod_1" || lister.calls != 1 {
		t.Fatalf("reloaded inside the TTL after %d loads", lister.calls)
	}
	now = now.Add(2 * time.Second)
	if m, _ := store.Current(ctx); mapped(m, "p1") != "prod_2" {
		t.Fatal("not reloaded after the TTL")
	}

	// Invalidate reloads at once.
	lister.rows = row("prod_3")
	store.Invalidate()
	if m, _ := store.Current(ctx); mapped(m, "p1") != "prod_3" {
		t.Fatal("not reloaded after Invalidate")
	}

	// After an edit, a failed reload does not fall back to the map from
	// before it.
	lister.err = errors.New("db down")
	store.Invalidate()
	if _, err := store.Current(ctx); err == nil {
		t.Fatal("served the map from before the edit")
	}
	lister.err = nil
	if m, err := store.Current(ctx); err != nil || mapped(m, "p1") != "prod_3" {
		t.Fatalf("reload after the edit: %v %v", mapped(m, "p1"), err)
	}

	// A routine refresh that fails serves the last good map and retries soon.
	lister.err = errors.New("db down")
	now = now.Add(productMapTTL + time.Second)
	m, err = store.Current(ctx)
	if err != nil || mapped(m, "p1") != "prod_3" {
		t.Fatalf("failed reload: %v %v", mapped(m, "p1"), err)
	}
	lister.err = nil
	lister.rows = nil
	now = now.Add(productMapRetry + time.Second)
	if m, _ := store.Current(ctx); m.Len() != 0 {
		t.Fatal("no retry after the failure")
	}
}

func TestSetProductMappingSellsAtOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	versions := repository.NewTenantTierVersionRepository(h.db)
	yearly := h.publishPrice(versions, "pro", "y2", "year", 29000, true)
	h.provider.products["prod_pro_y"] = product("prod_pro_y", "Pro yearly", "year", 29000, true, true)
	h.provider.products["prod_pro_y_old"] = product("prod_pro_y_old", "Pro yearly old", "year", 28000, false, true)

	list, err := h.svc.ProductMappingList(ctx, true)
	if err != nil || !list.Checked {
		t.Fatalf("list: %+v %v", list, err)
	}
	statuses := map[string]string{}
	for _, m := range list.Mappings {
		statuses[m.Price.ID] = m.Status
	}
	if statuses[yearly] != billingdom.MappingUnmapped || statuses[h.proPrice.ID] != billingdom.MappingOK || statuses[h.maxPrice.ID] != billingdom.MappingOK {
		t.Fatalf("statuses = %v", statuses)
	}
	items, _, err := h.svc.Attention(ctx, 50, "")
	if err != nil || len(items) != 1 || items[0].Reason != AttentionUnmappedPrice || items[0].PriceID != yearly || items[0].TenantID != "" {
		t.Fatalf("attention = %+v %v", items, err)
	}

	// The wrong amount is refused with a readable reason, and nothing is saved.
	_, err = h.svc.SetProductMapping(ctx, yearly, true, "prod_pro_y_old", "new yearly price")
	refused, ok := errors.AsType[*MappingRefusedError](err)
	if !ok || refused.Status != billingdom.MappingPriceMismatch || !strings.Contains(refused.Message, "280.00 EUR") || !strings.Contains(refused.Message, "290.00 EUR") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := h.svc.SetProductMapping(ctx, yearly, true, proProduct, "x"); !isRefused(err, billingdom.MappingProductTaken) {
		t.Fatalf("a product selling two prices in one currency: %v", err)
	}
	if _, err := h.svc.SetProductMapping(ctx, yearly, true, "prod_nope", "x"); !isRefused(err, billingdom.MappingProductMissing) {
		t.Fatalf("unknown product: %v", err)
	}
	if _, err := h.svc.SetProductMapping(ctx, yearly, true, "prod_pro_y", ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := h.svc.SetProductMapping(ctx, "nope", true, "prod_pro_y", "x"); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown price: %v", err)
	}
	draft := h.publishPrice(versions, "max", "draft", "year", 79000, false)
	if _, err := h.svc.SetProductMapping(ctx, draft, true, "prod_pro_y", "x"); !errors.Is(err, ErrPriceNotSellable) {
		t.Fatalf("draft price: %v", err)
	}

	// The right product is saved and sold through at once on this replica.
	m, err := h.svc.SetProductMapping(ctx, yearly, true, "prod_pro_y", "new yearly price")
	if err != nil || m.Status != billingdom.MappingOK || m.Row == nil || m.Row.Reason != "new yearly price" || m.Row.UpdatedBy != models.ProductMappingByHarbor || m.Row.Currency != "EUR" {
		t.Fatalf("set: %+v %v", m, err)
	}
	tenantID, userID, accountID := h.workspace("y")
	req := planReq(tenantID, userID, accountID, "ttv-pro-y2")
	req.PriceID = yearly
	if _, err := h.svc.ChangePlan(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := h.provider.checkouts[len(h.provider.checkouts)-1].ProductID; got != "prod_pro_y" {
		t.Fatalf("checkout product = %s", got)
	}
	if items, _, _ := h.svc.Attention(ctx, 50, ""); len(items) != 0 {
		t.Fatalf("attention after mapping = %+v", items)
	}

	// An archived product cannot be bought, and the price shows as unmapped.
	h.provider.mu.Lock()
	p := h.provider.products["prod_pro_y"]
	p.Archived = true
	h.provider.products["prod_pro_y"] = p
	h.provider.mu.Unlock()
	h.svc.products = map[string]cachedProduct{}
	if _, err := h.svc.ChangePlan(ctx, req); !errors.Is(err, ErrTierNotAvailable) {
		t.Fatalf("checkout of an archived product: %v", err)
	}
	if items, _, _ := h.svc.Attention(ctx, 50, ""); len(items) != 1 || items[0].PriceID != yearly {
		t.Fatalf("attention for an archived product = %+v", items)
	}

	// Unmapping makes the price unsellable again.
	if m, err := h.svc.SetProductMapping(ctx, yearly, true, "", "pulled"); err != nil || m.Status != billingdom.MappingUnmapped {
		t.Fatalf("unmap: %+v %v", m, err)
	}
	if _, err := h.svc.ChangePlan(ctx, req); !errors.Is(err, ErrTierNotAvailable) {
		t.Fatalf("checkout of an unmapped price: %v", err)
	}
}

func TestProductSellsOnePricePerCurrency(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	versions := repository.NewTenantTierVersionRepository(h.db)
	// A second Pro price in USD on its own version, sold by the same product.
	usd := "price-pro-usd"
	v := &models.TenantTierVersion{ID: "ttv-pro-usd", TierID: "pro", Version: 99, Status: models.TierVersionStatusDraft, Purchasable: true, Entitlements: map[string]any{}}
	if err := versions.Create(ctx, v, []models.TenantTierVersionPrice{{ID: usd, TierVersionID: v.ID, Currency: "USD", BillingInterval: "month", NetMinor: 3200}}); err != nil {
		t.Fatal(err)
	}
	if _, err := versions.Publish(ctx, v.ID, "test", h.supersedeOnSale(versions, "pro"), h.now); err != nil {
		t.Fatal(err)
	}
	h.provider.mu.Lock()
	p := h.provider.products[proProduct]
	p.Prices = append(p.Prices, billingdom.ProductPrice{Currency: "USD", AmountMinor: 3200})
	h.provider.products[proProduct] = p
	h.provider.mu.Unlock()

	if _, err := h.svc.SetProductMapping(ctx, usd, true, proProduct, "usd price"); err != nil {
		t.Fatalf("a second currency of one product: %v", err)
	}
	products, err := h.svc.ProviderProducts(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range products {
		if p.ID == proProduct && len(p.MappedPriceIDs) != 2 {
			t.Fatalf("pro sells %v", p.MappedPriceIDs)
		}
	}
	m, _ := h.svc.productMap(ctx)
	if eur, _ := m.Price(proProduct, "EUR"); eur != h.proPrice.ID {
		t.Fatalf("EUR resolves to %s", eur)
	}
	if got, _ := m.Price(proProduct, "usd"); got != usd {
		t.Fatalf("USD resolves to %s", got)
	}
}

func TestSetProductMappingFreeMonthTier(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.svc.SetFreeTrialTier(ctx, "pro", "launch"); err != nil {
		t.Fatal(err)
	}
	h.provider.products["prod_pro_nt"] = product("prod_pro_nt", "Pro monthly no trial", "month", 2900, false, true)
	if _, err := h.svc.SetProductMapping(ctx, h.proPrice.ID, true, "prod_pro_nt", "swap"); !isRefused(err, billingdom.MappingTrialMissing) {
		t.Fatalf("free-month tier without a trial: %v", err)
	}
	// Remapping a price to the product it already has is fine.
	if _, err := h.svc.SetProductMapping(ctx, h.proPrice.ID, true, proProduct, "same"); err != nil {
		t.Fatal(err)
	}
}

func TestProductMappingListReportsDrift(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	// A dashboard edit after mapping: Max's product changed price, Pro's was
	// deleted.
	h.provider.mu.Lock()
	p := h.provider.products[maxProduct]
	p.Prices = []billingdom.ProductPrice{{Currency: "EUR", AmountMinor: 8900}}
	h.provider.products[maxProduct] = p
	delete(h.provider.products, proProduct)
	h.provider.mu.Unlock()

	list, err := h.svc.ProductMappingList(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range list.Mappings {
		got[m.Price.ID] = m.Status
	}
	if got[h.proPrice.ID] != billingdom.MappingProductMissing || got[h.maxPrice.ID] != billingdom.MappingPriceMismatch {
		t.Fatalf("statuses = %v", got)
	}
	if bad, err := h.svc.CheckProducts(ctx); err != nil || bad != 2 {
		t.Fatalf("CheckProducts = %d %v", bad, err)
	}

	// An unreachable provider leaves the rows unchecked rather than failing.
	h.provider.catalogErr = errors.New("timeout")
	list, err = h.svc.ProductMappingList(ctx, true)
	if err != nil || list.Checked || list.Mappings[0].Status != billingdom.MappingUnchecked {
		t.Fatalf("unreachable: %+v %v", list, err)
	}
	if _, err := h.svc.CheckProducts(ctx); err == nil {
		t.Fatal("CheckProducts hid an unreachable provider")
	}
}

func TestLiveMapPreparedBeforeTheSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// Without the live token, production products cannot be listed and a
	// live mapping is saved unchecked.
	if _, err := h.svc.ProviderProducts(ctx, false); !errors.Is(err, ErrModeUnreadable) || !strings.Contains(err.Error(), "polar_access_token_live") {
		t.Fatalf("live products without a token: %v", err)
	}
	m, err := h.svc.SetProductMapping(ctx, h.proPrice.ID, false, "prod_live_pro", "prepare live")
	if err != nil || m.Status != billingdom.MappingUnchecked {
		t.Fatalf("unchecked live mapping: %+v %v", m, err)
	}
	// It does not leak into the sandbox map that sells today.
	if n, _ := h.svc.MappedPriceCount(ctx); n != 2 {
		t.Fatalf("sandbox map = %d prices", n)
	}
	if pm, _ := h.svc.productMap(ctx); mapped(pm, h.proPrice.ID) != proProduct {
		t.Fatal("live mapping changed the sandbox map")
	}

	// With the live token, production products are checked and suggested.
	h.provider.mu.Lock()
	h.provider.liveKey = true
	h.provider.products["prod_live_pro"] = product("prod_live_pro", "Pro monthly", "month", 2900, true, false)
	h.provider.products["prod_live_max"] = product("prod_live_max", "Max monthly", "month", 7900, false, false)
	h.provider.mu.Unlock()
	list, err := h.svc.ProductMappingList(ctx, false)
	if err != nil || !list.Checked {
		t.Fatalf("live list: %+v %v", list, err)
	}
	ps, err := h.svc.ProviderProducts(ctx, false)
	if err != nil || len(ps) != 2 || ps[0].ID != "prod_live_max" || len(ps[0].MappedPriceIDs) != 0 || ps[1].MappedPriceIDs[0] != h.proPrice.ID {
		t.Fatalf("live products = %+v %v", ps, err)
	}
	sg, err := h.svc.SuggestProductMappings(ctx, false)
	if err != nil || len(sg) != 1 || sg[0].PriceID != h.maxPrice.ID || sg[0].Product.ID != "prod_live_max" {
		t.Fatalf("live suggestions = %+v %v", sg, err)
	}
	// A mismatching live product is now refused.
	h.provider.products["prod_live_bad"] = product("prod_live_bad", "Bad", "month", 100, false, false)
	if _, err := h.svc.SetProductMapping(ctx, h.maxPrice.ID, false, "prod_live_bad", "x"); !isRefused(err, billingdom.MappingPriceMismatch) {
		t.Fatalf("live mismatch: %v", err)
	}
}

func TestSuggestProductMappingsBreaksTiesByName(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	versions := repository.NewTenantTierVersionRepository(h.db)
	proYear := h.publishPrice(versions, "pro", "y2", "year", 29000, true)
	maxYear := h.publishPrice(versions, "max", "y2", "year", 29000, true)
	h.provider.products["prod_a"] = product("prod_a", "Pro yearly", "year", 29000, true, true)
	h.provider.products["prod_b"] = product("prod_b", "Max annual", "year", 29000, true, true)
	sg, err := h.svc.SuggestProductMappings(ctx, true)
	if err != nil || len(sg) != 2 {
		t.Fatalf("suggestions = %+v %v", sg, err)
	}
	want := map[string]string{proYear: "prod_a", maxYear: "prod_b"}
	for _, s := range sg {
		if want[s.PriceID] != s.Product.ID {
			t.Fatalf("suggested %s for %s", s.Product.ID, s.PriceID)
		}
	}

	// Names that do not tell them apart: no guess.
	for _, id := range []string{"prod_a", "prod_b"} {
		p := h.provider.products[id]
		p.Name = "Plan annual"
		h.provider.products[id] = p
	}
	if sg, err := h.svc.SuggestProductMappings(ctx, true); err != nil || len(sg) != 0 {
		t.Fatalf("ambiguous suggestions = %+v %v", sg, err)
	}
}

func TestUpgradeKeepsTargetPriceWhenUnmappedMidway(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, _ := h.workspace("a")
	subscribe(t, h, tenantID, models.SubscriptionStatusActive)
	h.now = h.now.Add(time.Minute)

	// Harbor unmaps the target while the provider switch is in flight.
	h.provider.duringChange = func() {
		if _, err := h.svc.SetProductMapping(ctx, h.maxPrice.ID, true, "", "pulled"); err != nil {
			t.Error(err)
		}
	}
	if _, err := h.svc.ChangePlan(ctx, PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID}, TierID: "max"}); err != nil {
		t.Fatal(err)
	}
	m := h.mirror("s1")
	if m.ProviderProductID != maxProduct || m.ProviderVariantID != maxProduct || m.TierVersionPriceID == nil || *m.TierVersionPriceID != h.maxPrice.ID {
		t.Fatalf("mirror after upgrade = product %s price %v", m.ProviderProductID, m.TierVersionPriceID)
	}
	if got := h.currentVersion(tenantID); got != h.maxPrice.TierVersionID+"/"+models.AssignmentReasonUpgrade {
		t.Fatalf("assignment = %s", got)
	}
}

func isRefused(err error, status string) bool {
	refused, ok := errors.AsType[*MappingRefusedError](err)
	return ok && refused.Status == status
}
