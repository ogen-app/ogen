package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Product mapping errors.
var (
	// ErrUnknownPrice: no such tier version price row (404).
	ErrUnknownPrice = errors.New("unknown price")
	// ErrPriceNotSellable: the price belongs to a draft, retired or
	// non-purchasable version, so no product may be mapped to it.
	ErrPriceNotSellable = errors.New("the price is not sellable: its version is not published or cannot be bought")
	// ErrModeUnreadable: the mode's catalog needs a token that is not set.
	ErrModeUnreadable = errors.New("cannot read that billing mode")
)

// MappingRefusedError is a product that cannot sell the price.
type MappingRefusedError struct {
	Status  string
	Message string
}

func (e *MappingRefusedError) Error() string { return e.Message }

const (
	// productMapTTL bounds how long a replica sells through a stale map after
	// a change in Harbor.
	productMapTTL = 60 * time.Second
	// productMapRetry spaces reloads while the database is failing; the last
	// good map is served meanwhile.
	productMapRetry = 5 * time.Second
)

// ProductMappingLister reads one mode's product mappings.
type ProductMappingLister interface {
	ListProductMappings(ctx context.Context, testMode bool) ([]models.BillingProductMapping, error)
}

// ProductStore serves the current mode's product map from
// billing_product_mappings through a short cache. Build one per process and
// share it between the billing services, so a change saved over gRPC is seen
// by the HTTP app at once; other replicas reload within productMapTTL.
type ProductStore struct {
	repo     ProductMappingLister
	testMode bool
	now      func() time.Time

	mu      sync.Mutex
	m       billingdom.ProductMap
	loaded  bool
	expires time.Time
}

// NewProductStore builds a store for one mode.
func NewProductStore(repo ProductMappingLister, testMode bool) *ProductStore {
	return &ProductStore{repo: repo, testMode: testMode, now: time.Now}
}

// TestMode is the mode the store serves.
func (v *ProductStore) TestMode() bool { return v.testMode }

// Current returns the product map. A failed reload serves the last good map,
// so a database blip never makes everything unsellable.
func (v *ProductStore) Current(ctx context.Context) (billingdom.ProductMap, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if v.loaded && now.Before(v.expires) {
		return v.m, nil
	}
	m, err := v.load(ctx)
	if err != nil {
		if !v.loaded {
			return billingdom.ProductMap{}, err
		}
		slog.WarnContext(ctx, "billing: product map reload failed; serving the last one", logging.AttrComponent, "billing", logging.AttrError, err)
		v.expires = now.Add(productMapRetry)
		return v.m, nil
	}
	v.m, v.loaded, v.expires = m, true, now.Add(productMapTTL)
	return m, nil
}

// Invalidate makes the next Current reload.
func (v *ProductStore) Invalidate() {
	v.mu.Lock()
	v.expires = time.Time{}
	v.mu.Unlock()
}

func (v *ProductStore) load(ctx context.Context) (billingdom.ProductMap, error) {
	rows, err := v.repo.ListProductMappings(ctx, v.testMode)
	if err != nil {
		return billingdom.ProductMap{}, err
	}
	out := make([]billingdom.ProductMapping, 0, len(rows))
	for _, r := range rows {
		out = append(out, billingdom.ProductMapping{PriceID: r.TierVersionPriceID, ProductID: r.ProductID, Currency: r.Currency})
	}
	return billingdom.NewProductMap(out)
}

// productMap is the current mode's map.
func (s *Service) productMap(ctx context.Context) (billingdom.ProductMap, error) {
	return s.d.Products.Current(ctx)
}

// MappedPriceCount is how many prices the current mode can sell.
func (s *Service) MappedPriceCount(ctx context.Context) (int, error) {
	m, err := s.productMap(ctx)
	return m.Len(), err
}

// ProviderProduct is a product of the organization with what it sells.
type ProviderProduct struct {
	billingdom.Product
	// MappedPriceIDs are the prices it sells in its mode, one per currency.
	MappedPriceIDs []string
}

// ProductMappingView is one price row, its product in a mode and the check.
type ProductMappingView struct {
	Price         billingdom.Price
	FreeTrialTier bool
	// ProductID is "" when unmapped; Product is nil when unmapped or not
	// readable.
	ProductID string
	Product   *ProviderProduct
	Status    string
	Message   string
	Row       *models.BillingProductMapping
}

// ProductMappings is a mode's map as the operator sees it.
type ProductMappings struct {
	TestMode bool
	// Checked: the mode's catalog was read, so statuses come from the
	// provider.
	Checked  bool
	Mappings []ProductMappingView
	// CheckErr is why the catalog could not be read.
	CheckErr error
}

// catalog reads a mode's catalog, mapping a missing token to
// ErrModeUnreadable (another mode) or ErrUnavailable (the server's own).
func (s *Service) catalog(ctx context.Context, testMode bool) (billingdom.Catalog, error) {
	if !s.Enabled() {
		return billingdom.Catalog{}, ErrUnavailable
	}
	cat, err := s.d.Provider.Catalog(ctx, testMode)
	switch {
	case err == nil:
		return cat, nil
	case !errors.Is(err, billingdom.ErrNotConfigured):
		return billingdom.Catalog{}, providerErr(err)
	case testMode == s.d.Settings.TestMode:
		return billingdom.Catalog{}, ErrUnavailable
	case testMode:
		return billingdom.Catalog{}, fmt.Errorf("%w: the sandbox cannot be read once billing runs live", ErrModeUnreadable)
	}
	return billingdom.Catalog{}, fmt.Errorf("%w: set polar_access_token_live in Harbor → Secrets and POLAR_ORGANIZATION_ID_LIVE to read production products", ErrModeUnreadable)
}

// ProviderProducts lists the organization's recurring products in a mode,
// for the mapping picker.
func (s *Service) ProviderProducts(ctx context.Context, testMode bool) ([]ProviderProduct, error) {
	cat, err := s.catalog(ctx, testMode)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Repo.ListProductMappings(ctx, testMode)
	if err != nil {
		return nil, err
	}
	byProduct := map[string][]string{}
	for _, r := range rows {
		byProduct[r.ProductID] = append(byProduct[r.ProductID], r.TierVersionPriceID)
	}
	out := make([]ProviderProduct, 0, len(cat.Products))
	for _, p := range cat.Products {
		out = append(out, ProviderProduct{Product: p, MappedPriceIDs: byProduct[p.ID]})
	}
	return out, nil
}

// mappingState is everything a mode's checks read, loaded once.
type mappingState struct {
	testMode   bool
	rows       map[string]models.BillingProductMapping // by price id
	prices     []billingdom.Price                      // sellable or mapped
	sellable   map[string]bool                         // price ids a product may be mapped to
	freeTierID string
	cat        billingdom.Catalog
	catErr     error
}

func (s *Service) loadMappingState(ctx context.Context, testMode bool) (*mappingState, error) {
	if !s.Enabled() {
		return nil, ErrUnavailable
	}
	rows, err := s.d.Repo.ListProductMappings(ctx, testMode)
	if err != nil {
		return nil, err
	}
	st := &mappingState{testMode: testMode, rows: make(map[string]models.BillingProductMapping, len(rows))}
	for _, r := range rows {
		st.rows[r.TierVersionPriceID] = r
	}
	sellable, err := s.d.Repo.MappablePrices(ctx, nil)
	if err != nil {
		return nil, err
	}
	st.sellable = make(map[string]bool, len(sellable))
	for _, p := range sellable {
		st.sellable[p.ID] = true
	}
	// A mapped price stays listed after its version is retired: webhooks of
	// subscriptions still on it resolve through the mapping.
	if st.prices, err = s.d.Repo.MappablePrices(ctx, slices.Collect(maps.Keys(st.rows))); err != nil {
		return nil, err
	}
	marked, err := s.d.Repo.FreeTrialTier(ctx)
	if err != nil {
		return nil, err
	}
	if marked != nil {
		st.freeTierID = marked.ID
	}
	st.cat, st.catErr = s.catalog(ctx, testMode)
	if st.catErr != nil && !errors.Is(st.catErr, ErrModeUnreadable) && !errors.Is(st.catErr, ErrProvider) {
		return nil, st.catErr
	}
	return st, nil
}

// mappedTo lists the prices a product sells in this mode.
func (st *mappingState) mappedTo(productID string) []string {
	var out []string
	for priceID, r := range st.rows {
		if r.ProductID == productID {
			out = append(out, priceID)
		}
	}
	slices.Sort(out)
	return out
}

// view checks one price row against its mapped product.
func (st *mappingState) view(p billingdom.Price) ProductMappingView {
	out := ProductMappingView{Price: p, FreeTrialTier: p.TierID != "" && p.TierID == st.freeTierID}
	row, mapped := st.rows[p.ID]
	if !mapped {
		out.Status, out.Message = billingdom.MappingUnmapped, "Not mapped: create the product in Polar, then map it here."
		return out
	}
	out.Row, out.ProductID = &row, row.ProductID
	if st.catErr != nil {
		out.Status, out.Message = billingdom.MappingUnchecked, "Not checked: "+st.catErr.Error()
		return out
	}
	prod, ok := st.cat.Product(row.ProductID)
	if !ok {
		out.Status = billingdom.MappingProductMissing
		out.Message = fmt.Sprintf("Product %s is not a recurring product of the %s organization.", row.ProductID, modeLabel(st.testMode))
		return out
	}
	out.Product = &ProviderProduct{Product: prod, MappedPriceIDs: st.mappedTo(prod.ID)}
	out.Status, out.Message = billingdom.CheckMapping(p, prod, st.testMode, out.FreeTrialTier)
	return out
}

func (st *mappingState) list() ProductMappings {
	out := ProductMappings{TestMode: st.testMode, Checked: st.catErr == nil, CheckErr: st.catErr}
	for _, p := range st.prices {
		out.Mappings = append(out.Mappings, st.view(p))
	}
	return out
}

// ProductMappingList lists every sellable (or still mapped) price row of a
// mode with its product and the check against the provider.
func (s *Service) ProductMappingList(ctx context.Context, testMode bool) (ProductMappings, error) {
	st, err := s.loadMappingState(ctx, testMode)
	if err != nil {
		return ProductMappings{}, err
	}
	return st.list(), nil
}

// SetProductMapping maps a price row to a product in a mode, or unmaps it
// with an empty product id. The product must match the price row; a product
// of a mode that cannot be read yet (production, without its token) is saved
// unchecked and checked once billing runs in that mode.
func (s *Service) SetProductMapping(ctx context.Context, priceID string, testMode bool, productID, reason string) (ProductMappingView, error) {
	priceID, productID, reason = strings.TrimSpace(priceID), strings.TrimSpace(productID), strings.TrimSpace(reason)
	if reason == "" {
		return ProductMappingView{}, ErrReasonRequired
	}
	st, err := s.loadMappingState(ctx, testMode)
	if err != nil {
		return ProductMappingView{}, err
	}
	idx := slices.IndexFunc(st.prices, func(p billingdom.Price) bool { return p.ID == priceID })
	if idx < 0 {
		known, err := s.d.Repo.Prices(ctx, []string{priceID})
		if err != nil {
			return ProductMappingView{}, err
		}
		if _, ok := known[priceID]; ok {
			return ProductMappingView{}, ErrPriceNotSellable
		}
		return ProductMappingView{}, ErrUnknownPrice
	}
	price := st.prices[idx]

	if productID == "" {
		if _, err := s.d.Repo.DeleteProductMapping(ctx, priceID, testMode); err != nil {
			return ProductMappingView{}, err
		}
		delete(st.rows, priceID)
		s.mappingChanged(ctx, testMode, priceID, "", reason)
		return st.view(price), nil
	}

	if !st.sellable[priceID] {
		return ProductMappingView{}, ErrPriceNotSellable
	}
	if err := st.checkCandidate(price, productID); err != nil {
		return ProductMappingView{}, err
	}
	row := models.BillingProductMapping{
		TierVersionPriceID: priceID, TestMode: testMode, ProductID: productID, Currency: strings.ToUpper(price.Currency),
		UpdatedAt: s.now(), UpdatedBy: models.ProductMappingByHarbor, Reason: reason,
	}
	if err := s.d.Repo.UpsertProductMapping(ctx, &row); errors.Is(err, repository.ErrProductTaken) {
		return ProductMappingView{}, st.takenError(productID, price.Currency)
	} else if err != nil {
		return ProductMappingView{}, err
	}
	st.rows[priceID] = row
	s.mappingChanged(ctx, testMode, priceID, productID, reason)
	return st.view(price), nil
}

// checkCandidate refuses a product that cannot sell the price.
func (st *mappingState) checkCandidate(price billingdom.Price, productID string) error {
	for other, r := range st.rows {
		if r.ProductID == productID && other != price.ID && strings.EqualFold(r.Currency, price.Currency) {
			return st.takenError(productID, price.Currency)
		}
	}
	if errors.Is(st.catErr, ErrModeUnreadable) {
		return nil // saved unchecked; checked once the mode can be read
	}
	if st.catErr != nil {
		return st.catErr
	}
	prod, ok := st.cat.Product(productID)
	if !ok {
		return &MappingRefusedError{Status: billingdom.MappingProductMissing,
			Message: fmt.Sprintf("Product %s is not a recurring product of the %s organization.", productID, modeLabel(st.testMode))}
	}
	status, msg := billingdom.CheckMapping(price, prod, st.testMode, price.TierID == st.freeTierID)
	if status != billingdom.MappingOK {
		return &MappingRefusedError{Status: status, Message: msg}
	}
	return nil
}

func (st *mappingState) takenError(productID, currency string) error {
	currency = strings.ToUpper(currency)
	msg := fmt.Sprintf("Product %s already sells another price in %s; unmap it there first.", productID, currency)
	for priceID, r := range st.rows {
		if r.ProductID != productID || !strings.EqualFold(r.Currency, currency) {
			continue
		}
		if i := slices.IndexFunc(st.prices, func(p billingdom.Price) bool { return p.ID == priceID }); i >= 0 {
			p := st.prices[i]
			msg = fmt.Sprintf("Product %s already sells %s v%d %sly in %s (%s); unmap it there first.", productID, p.TierName, p.TierVersion, p.Interval, currency, priceID)
		}
	}
	return &MappingRefusedError{Status: billingdom.MappingProductTaken, Message: msg}
}

// mappingChanged applies a saved change: this process sells through it at
// once, other replicas within productMapTTL. The map is operator-wide, so
// the change is audited in the log and on the row rather than in a
// workspace's activity.
func (s *Service) mappingChanged(ctx context.Context, testMode bool, priceID, productID, reason string) {
	if testMode == s.d.Settings.TestMode {
		s.d.Products.Invalidate()
	}
	slog.InfoContext(ctx, "billing: product mapping changed", logging.AttrComponent, "billing",
		"test_mode", testMode, "price_id", priceID, "product_id", productID, "reason", reason)
}

// ProductSuggestion pairs an unmapped price with the one free product that
// matches it.
type ProductSuggestion struct {
	PriceID string
	Product ProviderProduct
}

// SuggestProductMappings proposes, for each unmapped price row, the free
// product that passes every check. Several matches are narrowed by name (the
// product names the tier and the interval); a product that would sell two
// prices in one currency is not suggested. Nothing is saved.
func (s *Service) SuggestProductMappings(ctx context.Context, testMode bool) ([]ProductSuggestion, error) {
	st, err := s.loadMappingState(ctx, testMode)
	if err != nil {
		return nil, err
	}
	if st.catErr != nil {
		return nil, st.catErr
	}
	taken := map[string]bool{} // product + currency
	for _, r := range st.rows {
		taken[r.ProductID+"\x00"+strings.ToUpper(r.Currency)] = true
	}
	var out []ProductSuggestion
	uses := map[string]int{}
	for _, p := range st.prices {
		if _, mapped := st.rows[p.ID]; mapped {
			continue
		}
		freeTier := p.TierID == st.freeTierID
		best, bestScore, tie := billingdom.Product{}, -1, false
		for _, prod := range st.cat.Products {
			if taken[prod.ID+"\x00"+strings.ToUpper(p.Currency)] {
				continue
			}
			if status, _ := billingdom.CheckMapping(p, prod, testMode, freeTier); status != billingdom.MappingOK {
				continue
			}
			switch score := nameScore(p, prod); {
			case score > bestScore:
				best, bestScore, tie = prod, score, false
			case score == bestScore:
				tie = true
			}
		}
		if bestScore < 0 || tie {
			continue
		}
		uses[best.ID+"\x00"+strings.ToUpper(p.Currency)]++
		out = append(out, ProductSuggestion{PriceID: p.ID, Product: ProviderProduct{Product: best, MappedPriceIDs: st.mappedTo(best.ID)}})
	}
	return slices.DeleteFunc(out, func(sg ProductSuggestion) bool {
		i := slices.IndexFunc(st.prices, func(p billingdom.Price) bool { return p.ID == sg.PriceID })
		return uses[sg.Product.ID+"\x00"+strings.ToUpper(st.prices[i].Currency)] > 1
	}), nil
}

// nameScore ranks how well a product's name describes a price: it names the
// tier and the interval.
func nameScore(p billingdom.Price, prod billingdom.Product) int {
	score := 0
	name := strings.ToLower(prod.Name)
	if p.TierName != "" && strings.Contains(name, strings.ToLower(p.TierName)) {
		score += 2
	}
	words := map[string][]string{"month": {"month"}, "year": {"year", "annual"}}[p.Interval]
	if slices.ContainsFunc(words, func(w string) bool { return strings.Contains(name, w) }) {
		score++
	}
	return score
}

// CheckProducts checks the current mode's map against the provider and logs
// every mapping that would not sell. It never fails boot or a publish: an
// edit in the provider dashboard must not take the API down. It returns how
// many mappings disagree.
func (s *Service) CheckProducts(ctx context.Context) (int, error) {
	if !s.Configured(ctx) {
		return 0, nil
	}
	list, err := s.ProductMappingList(ctx, s.d.Settings.TestMode)
	if err != nil {
		return 0, err
	}
	if list.CheckErr != nil {
		return 0, list.CheckErr
	}
	bad := 0
	for _, m := range list.Mappings {
		switch m.Status {
		case billingdom.MappingOK:
		case billingdom.MappingUnmapped:
			slog.WarnContext(ctx, "billing: sellable price has no product; it cannot be bought", logging.AttrComponent, "billing",
				"price_id", m.Price.ID, "tier", m.Price.TierName, "version", m.Price.TierVersion, "interval", m.Price.Interval)
		default:
			bad++
			slog.ErrorContext(ctx, "billing: product does not match its price row", logging.AttrComponent, "billing",
				"price_id", m.Price.ID, "product_id", m.ProductID, "status", m.Status, "detail", m.Message)
		}
	}
	productPriceMismatch.Set(int64(bad))
	return bad, nil
}

// unmappedPriceItems: sellable prices with no product in the current mode, or
// whose product was archived (read through the product cache; a product that
// cannot be read is not reported here, the mapping page shows it).
func (s *Service) unmappedPriceItems(ctx context.Context, _ time.Time) ([]AttentionItem, error) {
	if !s.Enabled() {
		return nil, nil
	}
	m, err := s.productMap(ctx)
	if err != nil {
		return nil, err
	}
	prices, err := s.d.Repo.MappablePrices(ctx, nil)
	if err != nil {
		return nil, err
	}
	var items []AttentionItem
	for _, p := range prices {
		productID, ok := m.Product(p.ID)
		if ok {
			prod, err := s.product(ctx, productID)
			ok = err != nil || !prod.Archived
		}
		if !ok {
			items = append(items, AttentionItem{Reason: AttentionUnmappedPrice, PriceID: p.ID, Since: p.PublishedAt})
		}
	}
	return items, nil
}

func modeLabel(test bool) string {
	if test {
		return "sandbox"
	}
	return "production"
}
