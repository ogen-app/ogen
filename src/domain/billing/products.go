package billing

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Product is a purchasable provider product: one billing interval, a fixed
// price in one or more currencies, and an optional free trial.
type Product struct {
	ID   string
	Name string
	// Interval is month | year for a recurring product; IntervalCount is how
	// many of them one billing period spans (0 reads as 1).
	Interval      string
	IntervalCount int
	Prices        []ProductPrice
	// HasFreeTrial and TrialInterval/TrialIntervalCount describe the trial
	// the product carries (e.g. month / 1).
	HasFreeTrial       bool
	TrialInterval      string
	TrialIntervalCount int
	// Archived products cannot be bought.
	Archived bool
	// VariablePricing: the product also bills something other than a fixed
	// price (seats, usage, an amount the customer picks). A checkout buys
	// the whole product, so such a product cannot sell a fixed price row.
	VariablePricing bool
	TestMode        bool
}

// ProductPrice is a product's fixed price in one currency.
type ProductPrice struct {
	Currency    string
	AmountMinor int64
}

// PriceIn returns the product's fixed price in a currency.
func (p Product) PriceIn(currency string) (int64, bool) {
	for _, pr := range p.Prices {
		if strings.EqualFold(pr.Currency, currency) {
			return pr.AmountMinor, true
		}
	}
	return 0, false
}

// OneMonthTrial reports whether the product carries exactly a one-month free
// trial: the free-month offer's precondition.
func (p Product) OneMonthTrial() bool {
	return p.HasFreeTrial && p.TrialInterval == "month" && p.TrialIntervalCount == 1
}

// TrialDays approximates the trial length in days for display.
func (p Product) TrialDays() int {
	if !p.HasFreeTrial {
		return 0
	}
	per := map[string]int{"day": 1, "week": 7, "month": 30, "year": 365}[p.TrialInterval]
	return per * p.TrialIntervalCount
}

// Catalog is an organization's recurring products in one mode.
type Catalog struct {
	TestMode bool
	Products []Product
}

// Product finds a product by id.
func (c Catalog) Product(id string) (Product, bool) {
	for _, p := range c.Products {
		if p.ID == id {
			return p, true
		}
	}
	return Product{}, false
}

// ProductMapping is one price row sold by a product in its currency.
type ProductMapping struct {
	PriceID   string
	ProductID string
	Currency  string
}

// ProductMap maps tier version price ids to the provider products that sell
// them, for one mode, and back: a product sells at most one price per
// currency. The billing_product_mappings table is its source.
type ProductMap struct {
	byPrice   map[string]ProductMapping
	byProduct map[string]map[string]string // product → currency → price
}

// NewProductMap builds a map. A product selling two prices in one currency is
// an error: the reverse lookup would be ambiguous.
func NewProductMap(rows []ProductMapping) (ProductMap, error) {
	m := ProductMap{byPrice: map[string]ProductMapping{}, byProduct: map[string]map[string]string{}}
	for _, r := range rows {
		r.PriceID, r.ProductID, r.Currency = strings.TrimSpace(r.PriceID), strings.TrimSpace(r.ProductID), strings.ToUpper(strings.TrimSpace(r.Currency))
		if r.PriceID == "" || r.ProductID == "" || r.Currency == "" {
			return ProductMap{}, fmt.Errorf("billing: empty price, product or currency")
		}
		byCurrency := m.byProduct[r.ProductID]
		if byCurrency == nil {
			byCurrency = map[string]string{}
			m.byProduct[r.ProductID] = byCurrency
		}
		if other, dup := byCurrency[r.Currency]; dup {
			return ProductMap{}, fmt.Errorf("billing: product %s sells both %s and %s in %s", r.ProductID, other, r.PriceID, r.Currency)
		}
		byCurrency[r.Currency] = r.PriceID
		m.byPrice[r.PriceID] = r
	}
	return m, nil
}

// Product returns the product mapped to a price.
func (m ProductMap) Product(priceID string) (string, bool) {
	r, ok := m.byPrice[priceID]
	return r.ProductID, ok
}

// Price returns the price a product sells in a currency. A product that
// sells a single price stands for it whatever the currency, so a snapshot
// without a currency still resolves.
func (m ProductMap) Price(productID, currency string) (string, bool) {
	byCurrency := m.byProduct[productID]
	if p, ok := byCurrency[strings.ToUpper(currency)]; ok {
		return p, true
	}
	if len(byCurrency) == 1 {
		for _, p := range byCurrency {
			return p, true
		}
	}
	return "", false
}

// PriceIDs lists every mapped price id.
func (m ProductMap) PriceIDs() []string {
	return slices.Sorted(maps.Keys(m.byPrice))
}

// Len is the number of mapped prices.
func (m ProductMap) Len() int { return len(m.byPrice) }
