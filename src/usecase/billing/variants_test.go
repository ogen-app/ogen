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
	rows  []models.BillingVariantMapping
	err   error
	calls int
}

func (f *fakeLister) ListVariantMappings(context.Context, bool) ([]models.BillingVariantMapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rows, f.err
}

func TestVariantStoreCachesAndKeepsLastGoodMap(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	lister := &fakeLister{rows: []models.BillingVariantMapping{{TierVersionPriceID: "p1", VariantID: "101"}}}
	store := NewVariantStore(lister, true)
	store.now = func() time.Time { return now }

	if _, err := NewVariantStore(&fakeLister{err: errors.New("down")}, true).Current(ctx); err == nil {
		t.Fatal("a store that never loaded served a map")
	}
	m, err := store.Current(ctx)
	if v, _ := m.Variant("p1"); err != nil || v != "101" {
		t.Fatalf("first load: %v %v", m, err)
	}

	// Within the TTL a change in the table is not seen; after it, it is.
	lister.rows = []models.BillingVariantMapping{{TierVersionPriceID: "p1", VariantID: "102"}}
	now = now.Add(variantMapTTL - time.Second)
	if m, _ := store.Current(ctx); m.Len() != 1 {
		t.Fatal("map lost")
	} else if v, _ := m.Variant("p1"); v != "101" || lister.calls != 1 {
		t.Fatalf("reloaded inside the TTL: %s after %d loads", v, lister.calls)
	}
	now = now.Add(2 * time.Second)
	if m, _ := store.Current(ctx); func() string { v, _ := m.Variant("p1"); return v }() != "102" {
		t.Fatal("not reloaded after the TTL")
	}

	// Invalidate reloads at once.
	lister.rows = []models.BillingVariantMapping{{TierVersionPriceID: "p1", VariantID: "103"}}
	store.Invalidate()
	if m, _ := store.Current(ctx); func() string { v, _ := m.Variant("p1"); return v }() != "103" {
		t.Fatal("not reloaded after Invalidate")
	}

	// A failed reload serves the last good map and retries soon.
	lister.err = errors.New("db down")
	store.Invalidate()
	m, err = store.Current(ctx)
	if v, _ := m.Variant("p1"); err != nil || v != "103" {
		t.Fatalf("failed reload: %v %v", v, err)
	}
	lister.err = nil
	lister.rows = nil
	now = now.Add(variantMapRetry + time.Second)
	if m, _ := store.Current(ctx); m.Len() != 0 {
		t.Fatal("no retry after the failure")
	}
}

func TestSeedVariantsImportsEnvOnce(t *testing.T) {
	h := newHarness(t, func(s *billingdom.Settings) {
		s.VariantSeed = `{"price-pro-m":"301","price-max-m":"302","price-gone":"303"}`
	})
	ctx := t.Context()

	// The harness already mapped the test mode, so the env is ignored.
	if err := h.svc.SeedVariants(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := h.repo.ListVariantMappings(ctx, true); len(rows) != 2 || rows[0].VariantID == "301" || rows[1].VariantID == "302" {
		t.Fatalf("seed overwrote Harbor's map: %+v", rows)
	}

	// An empty mode is seeded; unknown prices are skipped.
	if _, err := h.db.NewDelete().Model((*models.BillingVariantMapping)(nil)).Where("true").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h.svc.d.Variants.Invalidate()
	if err := h.svc.SeedVariants(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := h.repo.ListVariantMappings(ctx, true)
	if err != nil || len(rows) != 2 {
		t.Fatalf("seeded rows = %+v %v", rows, err)
	}
	for _, r := range rows {
		if r.UpdatedBy != models.VariantMappingByEnvSeed || !r.TestMode {
			t.Fatalf("seeded row = %+v", r)
		}
	}
	if n, err := h.svc.MappedPriceCount(ctx); err != nil || n != 2 {
		t.Fatalf("store after seed: %d %v", n, err)
	}
	// Live mode is untouched.
	if rows, _ := h.repo.ListVariantMappings(ctx, false); len(rows) != 0 {
		t.Fatalf("live rows = %+v", rows)
	}
}

func TestSetVariantMappingSellsAtOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	versions := repository.NewTenantTierVersionRepository(h.db)
	yearly := h.publishPrice(versions, "pro", "y2", "year", 29000, true)
	h.provider.variants["111"] = billingdom.Variant{ID: "111", ProductName: "Pro", Name: "Yearly", Status: billingdom.VariantPublished, PriceMinor: 29000, Interval: "year", IntervalCount: 1, HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1, TestMode: true}
	h.provider.variants["112"] = billingdom.Variant{ID: "112", ProductName: "Pro", Name: "Yearly old", Status: billingdom.VariantPublished, PriceMinor: 28000, Interval: "year", IntervalCount: 1, TestMode: true}

	list, err := h.svc.VariantMappingList(ctx, true)
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
	_, err = h.svc.SetVariantMapping(ctx, yearly, true, "112", "new yearly price")
	refused, ok := errors.AsType[*MappingRefusedError](err)
	if !ok || refused.Status != billingdom.MappingPriceMismatch || !strings.Contains(refused.Message, "280.00 EUR") || !strings.Contains(refused.Message, "290.00 EUR") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := h.svc.SetVariantMapping(ctx, yearly, true, proVariant, "x"); !isRefused(err, billingdom.MappingVariantTaken) {
		t.Fatalf("a variant selling two prices: %v", err)
	}
	if _, err := h.svc.SetVariantMapping(ctx, yearly, true, "999", "x"); !isRefused(err, billingdom.MappingVariantMissing) {
		t.Fatalf("unknown variant: %v", err)
	}
	if _, err := h.svc.SetVariantMapping(ctx, yearly, true, "111", ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := h.svc.SetVariantMapping(ctx, "nope", true, "111", "x"); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown price: %v", err)
	}
	draft := h.publishPrice(versions, "max", "draft", "year", 79000, false)
	if _, err := h.svc.SetVariantMapping(ctx, draft, true, "111", "x"); !errors.Is(err, ErrPriceNotSellable) {
		t.Fatalf("draft price: %v", err)
	}

	// The right variant is saved and sold through at once on this replica.
	m, err := h.svc.SetVariantMapping(ctx, yearly, true, "111", "new yearly price")
	if err != nil || m.Status != billingdom.MappingOK || m.Row == nil || m.Row.Reason != "new yearly price" || m.Row.UpdatedBy != models.VariantMappingByHarbor {
		t.Fatalf("set: %+v %v", m, err)
	}
	tenantID, userID, accountID := h.workspace("y")
	req := planReq(tenantID, userID, accountID, "ttv-pro-y2")
	req.PriceID = yearly
	if _, err := h.svc.ChangePlan(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := h.provider.checkouts[len(h.provider.checkouts)-1].VariantID; got != "111" {
		t.Fatalf("checkout variant = %s", got)
	}
	if items, _, _ := h.svc.Attention(ctx, 50, ""); len(items) != 0 {
		t.Fatalf("attention after mapping = %+v", items)
	}

	// Unmapping makes the price unsellable again.
	if m, err := h.svc.SetVariantMapping(ctx, yearly, true, "", "pulled"); err != nil || m.Status != billingdom.MappingUnmapped {
		t.Fatalf("unmap: %+v %v", m, err)
	}
	if _, err := h.svc.ChangePlan(ctx, req); !errors.Is(err, ErrTierNotAvailable) {
		t.Fatalf("checkout of an unmapped price: %v", err)
	}
}

func TestSetVariantMappingFreeMonthTier(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.svc.SetFreeTrialTier(ctx, "pro", "launch"); err != nil {
		t.Fatal(err)
	}
	h.provider.variants["121"] = billingdom.Variant{ID: "121", ProductName: "Pro", Name: "Monthly no trial", Status: billingdom.VariantPublished, PriceMinor: 2900, Interval: "month", IntervalCount: 1, TestMode: true}
	if _, err := h.svc.SetVariantMapping(ctx, h.proPrice.ID, true, "121", "swap"); !isRefused(err, billingdom.MappingTrialMissing) {
		t.Fatalf("free-month tier without a trial: %v", err)
	}
	// Remapping a price to the variant it already has is fine.
	if _, err := h.svc.SetVariantMapping(ctx, h.proPrice.ID, true, proVariant, "same"); err != nil {
		t.Fatal(err)
	}
}

func TestVariantMappingListReportsDrift(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	// An LS dashboard edit after mapping: Max's variant changed price, Pro's
	// variant was deleted.
	h.provider.mu.Lock()
	v := h.provider.variants[maxVariant]
	v.PriceMinor = 8900
	h.provider.variants[maxVariant] = v
	delete(h.provider.variants, proVariant)
	h.provider.mu.Unlock()

	list, err := h.svc.VariantMappingList(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range list.Mappings {
		got[m.Price.ID] = m.Status
	}
	if got[h.proPrice.ID] != billingdom.MappingVariantMissing || got[h.maxPrice.ID] != billingdom.MappingPriceMismatch {
		t.Fatalf("statuses = %v", got)
	}
	if bad, err := h.svc.CheckVariants(ctx); err != nil || bad != 2 {
		t.Fatalf("CheckVariants = %d %v", bad, err)
	}

	// An unreachable provider leaves the rows unchecked rather than failing.
	h.provider.catalogErr = errors.New("timeout")
	list, err = h.svc.VariantMappingList(ctx, true)
	if err != nil || list.Checked || list.Mappings[0].Status != billingdom.MappingUnchecked {
		t.Fatalf("unreachable: %+v %v", list, err)
	}
	if _, err := h.svc.CheckVariants(ctx); err == nil {
		t.Fatal("CheckVariants hid an unreachable provider")
	}
}

func TestLiveMapPreparedBeforeTheSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// Without the live key, live variants cannot be listed and a live
	// mapping is saved unchecked.
	if _, err := h.svc.ProviderVariants(ctx, false); !errors.Is(err, ErrModeUnreadable) || !strings.Contains(err.Error(), "lemonsqueezy_api_key_live") {
		t.Fatalf("live variants without a key: %v", err)
	}
	m, err := h.svc.SetVariantMapping(ctx, h.proPrice.ID, false, "901", "prepare live")
	if err != nil || m.Status != billingdom.MappingUnchecked {
		t.Fatalf("unchecked live mapping: %+v %v", m, err)
	}
	// It does not leak into the test map that sells today.
	if n, _ := h.svc.MappedPriceCount(ctx); n != 2 {
		t.Fatalf("test map = %d prices", n)
	}
	if v, _ := h.svc.variantMap(ctx); func() string { id, _ := v.Variant(h.proPrice.ID); return id }() != proVariant {
		t.Fatal("live mapping changed the test map")
	}

	// With the live key, live variants are checked and suggested.
	h.provider.mu.Lock()
	h.provider.liveKey = true
	h.provider.variants["901"] = billingdom.Variant{ID: "901", ProductName: "Pro", Name: "Monthly", Status: billingdom.VariantPublished, PriceMinor: 2900, Interval: "month", IntervalCount: 1, HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1}
	h.provider.variants["902"] = billingdom.Variant{ID: "902", ProductName: "Max", Name: "Monthly", Status: billingdom.VariantPublished, PriceMinor: 7900, Interval: "month", IntervalCount: 1}
	h.provider.mu.Unlock()
	list, err := h.svc.VariantMappingList(ctx, false)
	if err != nil || !list.Checked {
		t.Fatalf("live list: %+v %v", list, err)
	}
	vs, err := h.svc.ProviderVariants(ctx, false)
	if err != nil || len(vs) != 2 || vs[0].MappedPriceID != h.proPrice.ID || vs[1].MappedPriceID != "" {
		t.Fatalf("live variants = %+v %v", vs, err)
	}
	sg, err := h.svc.SuggestVariantMappings(ctx, false)
	if err != nil || len(sg) != 1 || sg[0].PriceID != h.maxPrice.ID || sg[0].Variant.ID != "902" {
		t.Fatalf("live suggestions = %+v %v", sg, err)
	}
	// A mismatching live variant is now refused.
	h.provider.variants["903"] = billingdom.Variant{ID: "903", Status: billingdom.VariantPublished, PriceMinor: 100, Interval: "month"}
	if _, err := h.svc.SetVariantMapping(ctx, h.maxPrice.ID, false, "903", "x"); !isRefused(err, billingdom.MappingPriceMismatch) {
		t.Fatalf("live mismatch: %v", err)
	}
}

func TestSuggestVariantMappingsBreaksTiesByName(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	versions := repository.NewTenantTierVersionRepository(h.db)
	proYear := h.publishPrice(versions, "pro", "y2", "year", 29000, true)
	maxYear := h.publishPrice(versions, "max", "y2", "year", 29000, true)
	for id, v := range map[string]billingdom.Variant{
		"131": {ProductName: "Pro", Name: "Yearly"},
		"132": {ProductName: "Max", Name: "Annual"},
	} {
		v.ID, v.Status, v.PriceMinor, v.Interval, v.IntervalCount, v.TestMode = id, billingdom.VariantPublished, 29000, "year", 1, true
		v.HasFreeTrial, v.TrialInterval, v.TrialIntervalCount = true, "month", 1
		h.provider.variants[id] = v
	}
	sg, err := h.svc.SuggestVariantMappings(ctx, true)
	if err != nil || len(sg) != 2 {
		t.Fatalf("suggestions = %+v %v", sg, err)
	}
	want := map[string]string{proYear: "131", maxYear: "132"}
	for _, s := range sg {
		if want[s.PriceID] != s.Variant.ID {
			t.Fatalf("suggested %s for %s", s.Variant.ID, s.PriceID)
		}
	}

	// Names that do not tell them apart: no guess.
	for _, id := range []string{"131", "132"} {
		v := h.provider.variants[id]
		v.ProductName, v.Name = "Plan", "Annual"
		h.provider.variants[id] = v
	}
	if sg, err := h.svc.SuggestVariantMappings(ctx, true); err != nil || len(sg) != 0 {
		t.Fatalf("ambiguous suggestions = %+v %v", sg, err)
	}
}

func isRefused(err error, status string) bool {
	refused, ok := errors.AsType[*MappingRefusedError](err)
	return ok && refused.Status == status
}
