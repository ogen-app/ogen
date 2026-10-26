package billing

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// VariantMap maps tier version price ids to provider variant ids, both ways,
// for one mode. The billing_variant_mappings table is its source; the
// deprecated LEMONSQUEEZY_VARIANTS env only seeds that table.
type VariantMap struct {
	byPrice   map[string]string
	byVariant map[string]string
}

// NewVariantMap builds a map from price id to variant id. Two prices mapped
// to one variant is an error: the reverse lookup would be ambiguous.
func NewVariantMap(byPrice map[string]string) (VariantMap, error) {
	m := VariantMap{byPrice: map[string]string{}, byVariant: map[string]string{}}
	for price, variant := range byPrice {
		price, variant = strings.TrimSpace(price), strings.TrimSpace(variant)
		if price == "" || variant == "" {
			return VariantMap{}, fmt.Errorf("billing: empty price or variant id")
		}
		if other, dup := m.byVariant[variant]; dup {
			return VariantMap{}, fmt.Errorf("billing: variant %s mapped to both %s and %s", variant, other, price)
		}
		m.byPrice[price] = variant
		m.byVariant[variant] = price
	}
	return m, nil
}

// ParseVariantMap parses the JSON LEMONSQUEEZY_VARIANTS value,
// {"<price_id>":"<variant_id>"}. An empty string is an empty map.
func ParseVariantMap(raw string) (VariantMap, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return NewVariantMap(nil)
	}
	var in map[string]string
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return VariantMap{}, fmt.Errorf("billing: LEMONSQUEEZY_VARIANTS: %w", err)
	}
	m, err := NewVariantMap(in)
	if err != nil {
		return VariantMap{}, fmt.Errorf("billing: LEMONSQUEEZY_VARIANTS: %w", err)
	}
	return m, nil
}

// Variant returns the variant mapped to a price.
func (m VariantMap) Variant(priceID string) (string, bool) {
	v, ok := m.byPrice[priceID]
	return v, ok
}

// Price returns the price a variant is mapped to.
func (m VariantMap) Price(variantID string) (string, bool) {
	p, ok := m.byVariant[variantID]
	return p, ok
}

// PriceIDs lists every mapped price id.
func (m VariantMap) PriceIDs() []string {
	return slices.Sorted(maps.Keys(m.byPrice))
}

// Len is the number of mapped prices.
func (m VariantMap) Len() int { return len(m.byPrice) }
