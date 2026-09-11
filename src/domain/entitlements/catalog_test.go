package entitlements_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/entitlements"
)

func TestLoadCatalog(t *testing.T) {
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cat.Features()) == 0 {
		t.Fatal("catalog is empty")
	}

	f, ok := cat.Get("team_seats")
	if !ok {
		t.Fatal("team_seats missing from catalog")
	}
	if f.ValueType != entitlements.ValueTypeNumeric || !f.IsMaterial || f.Reset != entitlements.ResetStanding {
		t.Fatalf("unexpected team_seats feature: %+v", f)
	}
	if b, ok := cat.Get("all_campaign_types"); !ok || b.ValueType != entitlements.ValueTypeBoolean {
		t.Fatalf("unexpected all_campaign_types feature: %+v (ok=%v)", b, ok)
	}
	if _, ok := cat.Get("does_not_exist"); ok {
		t.Fatal("Get returned a phantom key")
	}
}

func TestCatalogValidate(t *testing.T) {
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// A subset mirroring the seeded trial-v1 entitlements (numbers arrive as
	// float64 from jsonb; a null numeric = unlimited).
	valid := map[string]any{
		"team_seats":          float64(1),
		"posts_total":         float64(15),
		"all_campaign_types":  false,
		"media_storage_bytes": nil, // unlimited
	}
	if err := cat.Validate(valid); err != nil {
		t.Fatalf("valid entitlements rejected: %v", err)
	}

	cases := map[string]map[string]any{
		"numeric as bool": {"team_seats": true},
		"bool as number":  {"all_campaign_types": float64(1)},
		"unknown key":     {"nope": float64(1)},
		"null bool":       {"all_campaign_types": nil},
	}
	for name, ents := range cases {
		if err := cat.Validate(ents); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}
