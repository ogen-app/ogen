package billing

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// VariantMap maps tier version price ids to provider variant ids, both ways.
// It is built from LEMONSQUEEZY_VARIANTS: {"<price_id>":"<variant_id>"}.
type VariantMap struct {
	byPrice   map[string]string
	byVariant map[string]string
}

// ParseVariantMap parses the JSON variant map. An empty string is an empty map.
// Two prices mapped to one variant is an error: the reverse lookup would be
// ambiguous.
func ParseVariantMap(raw string) (VariantMap, error) {
	m := VariantMap{byPrice: map[string]string{}, byVariant: map[string]string{}}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return m, nil
	}
	var in map[string]string
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return VariantMap{}, fmt.Errorf("billing: LEMONSQUEEZY_VARIANTS: %w", err)
	}
	for price, variant := range in {
		price, variant = strings.TrimSpace(price), strings.TrimSpace(variant)
		if price == "" || variant == "" {
			return VariantMap{}, fmt.Errorf("billing: LEMONSQUEEZY_VARIANTS: empty price or variant id")
		}
		if other, dup := m.byVariant[variant]; dup {
			return VariantMap{}, fmt.Errorf("billing: LEMONSQUEEZY_VARIANTS: variant %s mapped to both %s and %s", variant, other, price)
		}
		m.byPrice[price] = variant
		m.byVariant[variant] = price
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
