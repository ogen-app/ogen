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

// Variant mapping errors.
var (
	// ErrUnknownPrice: no such tier version price row (404).
	ErrUnknownPrice = errors.New("unknown price")
	// ErrPriceNotSellable: the price belongs to a draft, retired or
	// non-purchasable version, so no variant may be mapped to it.
	ErrPriceNotSellable = errors.New("the price is not sellable: its version is not published or cannot be bought")
	// ErrModeUnreadable: the mode's catalog needs an API key that is not set.
	ErrModeUnreadable = errors.New("cannot read that Lemon Squeezy mode")
)

// MappingRefusedError is a variant that cannot sell the price.
type MappingRefusedError struct {
	Status  string
	Message string
}

func (e *MappingRefusedError) Error() string { return e.Message }

const (
	// variantMapTTL bounds how long a replica sells through a stale map after
	// a change in Harbor.
	variantMapTTL = 60 * time.Second
	// variantMapRetry spaces reloads while the database is failing; the last
	// good map is served meanwhile.
	variantMapRetry = 5 * time.Second
)

// VariantMappingLister reads one mode's variant mappings.
type VariantMappingLister interface {
	ListVariantMappings(ctx context.Context, testMode bool) ([]models.BillingVariantMapping, error)
}

// VariantStore serves the current mode's variant map from
// billing_variant_mappings through a short cache. Build one per process and
// share it between the billing services, so a change saved over gRPC is seen
// by the HTTP app at once; other replicas reload within variantMapTTL.
type VariantStore struct {
	repo     VariantMappingLister
	testMode bool
	now      func() time.Time

	mu      sync.Mutex
	m       billingdom.VariantMap
	loaded  bool
	expires time.Time
}

// NewVariantStore builds a store for one mode.
func NewVariantStore(repo VariantMappingLister, testMode bool) *VariantStore {
	return &VariantStore{repo: repo, testMode: testMode, now: time.Now}
}

// TestMode is the mode the store serves.
func (v *VariantStore) TestMode() bool { return v.testMode }

// Current returns the variant map. A failed reload serves the last good map,
// so a database blip never makes everything unsellable.
func (v *VariantStore) Current(ctx context.Context) (billingdom.VariantMap, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if v.loaded && now.Before(v.expires) {
		return v.m, nil
	}
	m, err := v.load(ctx)
	if err != nil {
		if !v.loaded {
			return billingdom.VariantMap{}, err
		}
		slog.WarnContext(ctx, "billing: variant map reload failed; serving the last one", logging.AttrComponent, "billing", logging.AttrError, err)
		v.expires = now.Add(variantMapRetry)
		return v.m, nil
	}
	v.m, v.loaded, v.expires = m, true, now.Add(variantMapTTL)
	return m, nil
}

// Invalidate makes the next Current reload.
func (v *VariantStore) Invalidate() {
	v.mu.Lock()
	v.expires = time.Time{}
	v.mu.Unlock()
}

func (v *VariantStore) load(ctx context.Context) (billingdom.VariantMap, error) {
	rows, err := v.repo.ListVariantMappings(ctx, v.testMode)
	if err != nil {
		return billingdom.VariantMap{}, err
	}
	byPrice := make(map[string]string, len(rows))
	for _, r := range rows {
		byPrice[r.TierVersionPriceID] = r.VariantID
	}
	return billingdom.NewVariantMap(byPrice)
}

// variantMap is the current mode's map.
func (s *Service) variantMap(ctx context.Context) (billingdom.VariantMap, error) {
	return s.d.Variants.Current(ctx)
}

// MappedPriceCount is how many prices the current mode can sell.
func (s *Service) MappedPriceCount(ctx context.Context) (int, error) {
	m, err := s.variantMap(ctx)
	return m.Len(), err
}

// SeedVariants imports the deprecated LEMONSQUEEZY_VARIANTS value into the
// table, once: only while the current mode has no mapping. Afterwards the env
// is ignored and Harbor owns the map.
func (s *Service) SeedVariants(ctx context.Context) error {
	seed := s.d.Settings.VariantSeed
	if !s.Enabled() || seed == "" {
		return nil
	}
	m, err := billingdom.ParseVariantMap(seed)
	if err != nil {
		return err
	}
	rows := make([]models.BillingVariantMapping, 0, m.Len())
	for _, priceID := range m.PriceIDs() {
		variantID, _ := m.Variant(priceID)
		rows = append(rows, models.BillingVariantMapping{
			TierVersionPriceID: priceID, VariantID: variantID, UpdatedAt: s.now(),
			UpdatedBy: models.VariantMappingByEnvSeed, Reason: "imported from LEMONSQUEEZY_VARIANTS",
		})
	}
	inserted, unknown, err := s.d.Repo.SeedVariantMappings(ctx, s.d.Settings.TestMode, rows)
	if err != nil {
		return err
	}
	for _, id := range unknown {
		slog.WarnContext(ctx, "billing: LEMONSQUEEZY_VARIANTS names an unknown price; not imported", logging.AttrComponent, "billing", "price_id", id)
	}
	if inserted > 0 {
		s.d.Variants.Invalidate()
		slog.InfoContext(ctx, "billing: variant mappings imported from LEMONSQUEEZY_VARIANTS; map them in Harbor from now on and remove the env",
			logging.AttrComponent, "billing", "test_mode", s.d.Settings.TestMode, "mappings", inserted)
		return nil
	}
	if len(unknown) == 0 {
		slog.WarnContext(ctx, "billing: LEMONSQUEEZY_VARIANTS is ignored: variants are mapped in Harbor; remove the env",
			logging.AttrComponent, "billing")
	}
	return nil
}

// ProviderVariant is a variant of the store with what it sells.
type ProviderVariant struct {
	billingdom.Variant
	Currency string
	// MappedPriceID is the price it sells in its mode, or "".
	MappedPriceID string
}

// VariantMapping is one price row, its variant in a mode and the check.
type VariantMapping struct {
	Price         billingdom.Price
	FreeTrialTier bool
	// VariantID is "" when unmapped; Variant is nil when unmapped or not
	// readable.
	VariantID string
	Variant   *ProviderVariant
	Status    string
	Message   string
	Row       *models.BillingVariantMapping
}

// VariantMappings is a mode's map as the operator sees it.
type VariantMappings struct {
	TestMode bool
	// Checked: the mode's catalog was read, so statuses come from the
	// provider.
	Checked  bool
	Mappings []VariantMapping
	// CheckErr is why the catalog could not be read.
	CheckErr error
}

// catalog reads a mode's catalog, mapping a missing key to ErrModeUnreadable
// (another mode) or ErrUnavailable (the server's own).
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
		return billingdom.Catalog{}, fmt.Errorf("%w: test mode cannot be read once billing runs live", ErrModeUnreadable)
	}
	return billingdom.Catalog{}, fmt.Errorf("%w: set lemonsqueezy_api_key_live in Harbor → Secrets to read live variants", ErrModeUnreadable)
}

// ProviderVariants lists the store's subscription variants in a mode, for
// the mapping picker.
func (s *Service) ProviderVariants(ctx context.Context, testMode bool) ([]ProviderVariant, error) {
	cat, err := s.catalog(ctx, testMode)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Repo.ListVariantMappings(ctx, testMode)
	if err != nil {
		return nil, err
	}
	byVariant := map[string]string{}
	for _, r := range rows {
		byVariant[r.VariantID] = r.TierVersionPriceID
	}
	out := make([]ProviderVariant, 0, len(cat.Variants))
	for _, v := range cat.Variants {
		out = append(out, ProviderVariant{Variant: v, Currency: cat.Currency, MappedPriceID: byVariant[v.ID]})
	}
	return out, nil
}

// mappingState is everything a mode's checks read, loaded once.
type mappingState struct {
	testMode   bool
	rows       map[string]models.BillingVariantMapping // by price id
	prices     []billingdom.Price                      // sellable or mapped
	sellable   map[string]bool                         // price ids a variant may be mapped to
	freeTierID string
	cat        billingdom.Catalog
	catErr     error
}

func (s *Service) loadMappingState(ctx context.Context, testMode bool) (*mappingState, error) {
	if !s.Enabled() {
		return nil, ErrUnavailable
	}
	rows, err := s.d.Repo.ListVariantMappings(ctx, testMode)
	if err != nil {
		return nil, err
	}
	st := &mappingState{testMode: testMode, rows: make(map[string]models.BillingVariantMapping, len(rows))}
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

// view checks one price row against its mapped variant.
func (st *mappingState) view(p billingdom.Price) VariantMapping {
	out := VariantMapping{Price: p, FreeTrialTier: p.TierID != "" && p.TierID == st.freeTierID}
	row, mapped := st.rows[p.ID]
	if !mapped {
		out.Status, out.Message = billingdom.MappingUnmapped, "Not mapped: create the variant in Lemon Squeezy, then map it here."
		return out
	}
	out.Row, out.VariantID = &row, row.VariantID
	if st.catErr != nil {
		out.Status, out.Message = billingdom.MappingUnchecked, "Not checked: "+st.catErr.Error()
		return out
	}
	v, ok := st.cat.Variant(row.VariantID)
	if !ok {
		out.Status = billingdom.MappingVariantMissing
		out.Message = fmt.Sprintf("Variant %s is not a subscription variant of the %s store.", row.VariantID, modeLabel(st.testMode))
		return out
	}
	out.Variant = &ProviderVariant{Variant: v, Currency: st.cat.Currency, MappedPriceID: p.ID}
	out.Status, out.Message = billingdom.CheckMapping(p, v, st.cat.Currency, st.testMode, out.FreeTrialTier)
	return out
}

func (st *mappingState) list() VariantMappings {
	out := VariantMappings{TestMode: st.testMode, Checked: st.catErr == nil, CheckErr: st.catErr}
	for _, p := range st.prices {
		out.Mappings = append(out.Mappings, st.view(p))
	}
	return out
}

// VariantMappingList lists every sellable (or still mapped) price row of a
// mode with its variant and the check against the provider.
func (s *Service) VariantMappingList(ctx context.Context, testMode bool) (VariantMappings, error) {
	st, err := s.loadMappingState(ctx, testMode)
	if err != nil {
		return VariantMappings{}, err
	}
	return st.list(), nil
}

// SetVariantMapping maps a price row to a variant in a mode, or unmaps it
// with an empty variant id. The variant must match the price row; a variant
// of a mode that cannot be read yet (live, without its key) is saved
// unchecked and checked once billing runs in that mode.
func (s *Service) SetVariantMapping(ctx context.Context, priceID string, testMode bool, variantID, reason string) (VariantMapping, error) {
	priceID, variantID, reason = strings.TrimSpace(priceID), strings.TrimSpace(variantID), strings.TrimSpace(reason)
	if reason == "" {
		return VariantMapping{}, ErrReasonRequired
	}
	st, err := s.loadMappingState(ctx, testMode)
	if err != nil {
		return VariantMapping{}, err
	}
	idx := slices.IndexFunc(st.prices, func(p billingdom.Price) bool { return p.ID == priceID })
	if idx < 0 {
		known, err := s.d.Repo.Prices(ctx, []string{priceID})
		if err != nil {
			return VariantMapping{}, err
		}
		if _, ok := known[priceID]; ok {
			return VariantMapping{}, ErrPriceNotSellable
		}
		return VariantMapping{}, ErrUnknownPrice
	}
	price := st.prices[idx]

	if variantID == "" {
		if _, err := s.d.Repo.DeleteVariantMapping(ctx, priceID, testMode); err != nil {
			return VariantMapping{}, err
		}
		delete(st.rows, priceID)
		s.mappingChanged(ctx, testMode, priceID, "", reason)
		return st.view(price), nil
	}

	if !st.sellable[priceID] {
		return VariantMapping{}, ErrPriceNotSellable
	}
	if err := st.checkCandidate(price, variantID); err != nil {
		return VariantMapping{}, err
	}
	row := models.BillingVariantMapping{
		TierVersionPriceID: priceID, TestMode: testMode, VariantID: variantID,
		UpdatedAt: s.now(), UpdatedBy: models.VariantMappingByHarbor, Reason: reason,
	}
	if err := s.d.Repo.UpsertVariantMapping(ctx, &row); errors.Is(err, repository.ErrVariantTaken) {
		return VariantMapping{}, st.takenError(variantID)
	} else if err != nil {
		return VariantMapping{}, err
	}
	st.rows[priceID] = row
	s.mappingChanged(ctx, testMode, priceID, variantID, reason)
	return st.view(price), nil
}

// checkCandidate refuses a variant that cannot sell the price.
func (st *mappingState) checkCandidate(price billingdom.Price, variantID string) error {
	for other, r := range st.rows {
		if r.VariantID == variantID && other != price.ID {
			return st.takenError(variantID)
		}
	}
	if errors.Is(st.catErr, ErrModeUnreadable) {
		return nil // saved unchecked; checked once the mode can be read
	}
	if st.catErr != nil {
		return st.catErr
	}
	v, ok := st.cat.Variant(variantID)
	if !ok {
		return &MappingRefusedError{Status: billingdom.MappingVariantMissing,
			Message: fmt.Sprintf("Variant %s is not a subscription variant of the %s store.", variantID, modeLabel(st.testMode))}
	}
	status, msg := billingdom.CheckMapping(price, v, st.cat.Currency, st.testMode, price.TierID == st.freeTierID)
	if status != billingdom.MappingOK {
		return &MappingRefusedError{Status: status, Message: msg}
	}
	return nil
}

func (st *mappingState) takenError(variantID string) error {
	msg := fmt.Sprintf("Variant %s already sells another price; unmap it there first.", variantID)
	for priceID, r := range st.rows {
		if r.VariantID != variantID {
			continue
		}
		if i := slices.IndexFunc(st.prices, func(p billingdom.Price) bool { return p.ID == priceID }); i >= 0 {
			p := st.prices[i]
			msg = fmt.Sprintf("Variant %s already sells %s v%d %sly (%s); unmap it there first.", variantID, p.TierName, p.TierVersion, p.Interval, priceID)
		}
	}
	return &MappingRefusedError{Status: billingdom.MappingVariantTaken, Message: msg}
}

// mappingChanged applies a saved change: this process sells through it at
// once, other replicas within variantMapTTL. The map is operator-wide, so
// the change is audited in the log and on the row rather than in a
// workspace's activity.
func (s *Service) mappingChanged(ctx context.Context, testMode bool, priceID, variantID, reason string) {
	if testMode == s.d.Settings.TestMode {
		s.d.Variants.Invalidate()
	}
	slog.InfoContext(ctx, "billing: variant mapping changed", logging.AttrComponent, "billing",
		"test_mode", testMode, "price_id", priceID, "variant_id", variantID, "reason", reason)
}

// VariantSuggestion pairs an unmapped price with the one free variant that
// matches it.
type VariantSuggestion struct {
	PriceID string
	Variant ProviderVariant
}

// SuggestVariantMappings proposes, for each unmapped price row, the free
// variant that passes every check. Several matches are narrowed by names
// (the product names the tier, the variant names the interval); a variant
// that would sell two prices is not suggested. Nothing is saved.
func (s *Service) SuggestVariantMappings(ctx context.Context, testMode bool) ([]VariantSuggestion, error) {
	st, err := s.loadMappingState(ctx, testMode)
	if err != nil {
		return nil, err
	}
	if st.catErr != nil {
		return nil, st.catErr
	}
	taken := map[string]bool{}
	for _, r := range st.rows {
		taken[r.VariantID] = true
	}
	var out []VariantSuggestion
	uses := map[string]int{}
	for _, p := range st.prices {
		if _, mapped := st.rows[p.ID]; mapped {
			continue
		}
		freeTier := p.TierID == st.freeTierID
		best, bestScore, tie := billingdom.Variant{}, -1, false
		for _, v := range st.cat.Variants {
			if taken[v.ID] {
				continue
			}
			if status, _ := billingdom.CheckMapping(p, v, st.cat.Currency, testMode, freeTier); status != billingdom.MappingOK {
				continue
			}
			switch score := nameScore(p, v); {
			case score > bestScore:
				best, bestScore, tie = v, score, false
			case score == bestScore:
				tie = true
			}
		}
		if bestScore < 0 || tie {
			continue
		}
		uses[best.ID]++
		out = append(out, VariantSuggestion{PriceID: p.ID, Variant: ProviderVariant{Variant: best, Currency: st.cat.Currency}})
	}
	return slices.DeleteFunc(out, func(sg VariantSuggestion) bool { return uses[sg.Variant.ID] > 1 }), nil
}

// nameScore ranks how well a variant's names describe a price: the product
// names the tier, the variant names the interval.
func nameScore(p billingdom.Price, v billingdom.Variant) int {
	score := 0
	if p.TierName != "" && strings.Contains(strings.ToLower(v.ProductName), strings.ToLower(p.TierName)) {
		score += 2
	}
	name := strings.ToLower(v.Name)
	words := map[string][]string{"month": {"month"}, "year": {"year", "annual"}}[p.Interval]
	if slices.ContainsFunc(words, func(w string) bool { return strings.Contains(name, w) }) {
		score++
	}
	return score
}

// CheckVariants checks the current mode's map against the provider and logs
// every mapping that would not sell. It never fails boot or a publish: an
// edit in the provider dashboard must not take the API down. It returns how
// many mappings disagree.
func (s *Service) CheckVariants(ctx context.Context) (int, error) {
	if !s.Configured(ctx) {
		return 0, nil
	}
	list, err := s.VariantMappingList(ctx, s.d.Settings.TestMode)
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
			slog.WarnContext(ctx, "billing: sellable price has no variant; it cannot be bought", logging.AttrComponent, "billing",
				"price_id", m.Price.ID, "tier", m.Price.TierName, "version", m.Price.TierVersion, "interval", m.Price.Interval)
		default:
			bad++
			slog.ErrorContext(ctx, "billing: variant does not match its price row", logging.AttrComponent, "billing",
				"price_id", m.Price.ID, "variant_id", m.VariantID, "status", m.Status, "detail", m.Message)
		}
	}
	variantPriceMismatch.Set(int64(bad))
	return bad, nil
}

// unmappedPriceItems: sellable prices with no variant in the current mode.
func (s *Service) unmappedPriceItems(ctx context.Context, _ time.Time) ([]AttentionItem, error) {
	if !s.Enabled() {
		return nil, nil
	}
	m, err := s.variantMap(ctx)
	if err != nil {
		return nil, err
	}
	prices, err := s.d.Repo.MappablePrices(ctx, nil)
	if err != nil {
		return nil, err
	}
	var items []AttentionItem
	for _, p := range prices {
		if _, ok := m.Variant(p.ID); !ok {
			items = append(items, AttentionItem{Reason: AttentionUnmappedPrice, PriceID: p.ID, Since: p.PublishedAt})
		}
	}
	return items, nil
}

func modeLabel(test bool) string {
	if test {
		return "test-mode"
	}
	return "live"
}
