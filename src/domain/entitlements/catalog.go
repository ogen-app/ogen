// Package entitlements holds CON-243's tier-entitlement layer: the
// engineering-owned feature catalog (an embedded JSON config), the point-in-time
// resolver that maps a tenant to the tier version in force at an instant, and the
// enriched views the public pricing + in-app entitlement endpoints return.
package entitlements

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// value_type values a Feature may declare. v1 supports numeric and boolean; enum
// (campaign-type breadth) and multiplier are reserved for a later catalog
// revision — the PRD models both with these two for now.
const (
	ValueTypeNumeric = "numeric"
	ValueTypeBoolean = "boolean"
)

// reset semantics for a numeric feature — how its counter behaves. Consumed by
// the limitations engine (CON-295); "" for boolean features.
const (
	ResetStanding = "standing" // a live ceiling (seats, storage, connected accounts)
	ResetMonthly  = "monthly"  // resets each calendar month (plan runs)
	ResetTotal    = "total"    // a lifetime cap that never resets (Trial post cap)
	ResetPerPost  = "per_post" // a per-post allowance (quality reviews)
)

// Feature is one catalog entry: the definition of an entitlement key, its type,
// and metadata. Engineering-owned (this package's JSON), never edited by
// operators — they edit per-version *values* via Harbor (CON-294).
type Feature struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	LinearIssue string `json:"linear_issue"`
	Status      string `json:"status"`
	ValueType   string `json:"value_type"`
	IsMaterial  bool   `json:"is_material"`
	Reset       string `json:"reset,omitempty"`
	Description string `json:"description"`
}

// Catalog is the ordered, validated set of features loaded from catalog.json.
type Catalog struct {
	features []Feature
	byKey    map[string]Feature
}

//go:embed catalog.json
var catalogJSON []byte

// loadCatalog parses + validates the embedded catalog once, memoizing the result
// (and any error) so repeated LoadCatalog calls are free.
var loadCatalog = sync.OnceValues(func() (*Catalog, error) {
	var features []Feature
	if err := json.Unmarshal(catalogJSON, &features); err != nil {
		return nil, fmt.Errorf("entitlements: parse catalog.json: %w", err)
	}
	byKey := make(map[string]Feature, len(features))
	for _, f := range features {
		switch {
		case f.Key == "":
			return nil, fmt.Errorf("entitlements: catalog entry %q has an empty key", f.Name)
		case f.ValueType != ValueTypeNumeric && f.ValueType != ValueTypeBoolean:
			return nil, fmt.Errorf("entitlements: catalog key %q has invalid value_type %q", f.Key, f.ValueType)
		}
		if _, dup := byKey[f.Key]; dup {
			return nil, fmt.Errorf("entitlements: duplicate catalog key %q", f.Key)
		}
		byKey[f.Key] = f
	}
	return &Catalog{features: features, byKey: byKey}, nil
})

// LoadCatalog parses and validates the embedded feature catalog (memoized).
func LoadCatalog() (*Catalog, error) { return loadCatalog() }

// Features returns the catalog features in declaration order.
func (c *Catalog) Features() []Feature { return c.features }

// Get returns the feature for key and whether it exists.
func (c *Catalog) Get(key string) (Feature, bool) {
	f, ok := c.byKey[key]
	return f, ok
}

// Validate checks a tier version's entitlement map against the catalog: every key
// must be known, and every value must match its feature's value_type (a numeric
// value may be nil = unlimited; a boolean must be a real bool). It is the
// boot/authoring guard that keeps a stored version honest. Values arrive as the
// map bun decodes from the jsonb column, so numbers are float64.
func (c *Catalog) Validate(entitlements map[string]any) error {
	for key, val := range entitlements {
		f, ok := c.byKey[key]
		if !ok {
			return fmt.Errorf("entitlements: unknown feature key %q", key)
		}
		switch f.ValueType {
		case ValueTypeNumeric:
			if val == nil {
				continue // null = unlimited
			}
			if _, ok := val.(float64); !ok {
				return fmt.Errorf("entitlements: key %q expects a number or null, got %T", key, val)
			}
		case ValueTypeBoolean:
			if _, ok := val.(bool); !ok {
				return fmt.Errorf("entitlements: key %q expects a bool, got %T", key, val)
			}
		}
	}
	return nil
}
