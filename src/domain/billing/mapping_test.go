package billing

import (
	"strings"
	"testing"
)

func TestCheckMapping(t *testing.T) {
	price := Price{ID: "p", TierName: "Pro", Currency: "EUR", Interval: "month", NetMinor: 5000}
	good := Variant{ID: "101", PriceMinor: 5000, Interval: "month", IntervalCount: 1, TestMode: true, HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1}
	with := func(f func(*Variant)) Variant { v := good; f(&v); return v }

	for _, tc := range []struct {
		name     string
		v        Variant
		currency string
		freeTier bool
		want     string
		message  string
	}{
		{name: "match", v: good, currency: "eur", freeTier: true, want: MappingOK},
		{name: "published", v: with(func(v *Variant) { v.Status = VariantPublished }), currency: "EUR", want: MappingOK},
		{name: "draft", v: with(func(v *Variant) { v.Status = "draft" }), currency: "EUR", want: MappingVariantMissing, message: "is draft"},
		{name: "count zero reads as one", v: with(func(v *Variant) { v.IntervalCount = 0 }), currency: "EUR", want: MappingOK},
		{name: "other mode", v: with(func(v *Variant) { v.TestMode = false }), currency: "EUR", want: MappingModeMismatch, message: "live variant"},
		{name: "yearly", v: with(func(v *Variant) { v.Interval = "year" }), currency: "EUR", want: MappingIntervalMismatch, message: "every year"},
		{name: "quarterly", v: with(func(v *Variant) { v.IntervalCount = 3 }), currency: "EUR", want: MappingIntervalMismatch, message: "every 3 months"},
		{name: "one-off", v: with(func(v *Variant) { v.Interval = "" }), currency: "EUR", want: MappingIntervalMismatch, message: "not a subscription"},
		{name: "currency", v: good, currency: "USD", want: MappingCurrencyMismatch, message: "sells in USD"},
		{name: "amount", v: with(func(v *Variant) { v.PriceMinor = 4900 }), currency: "EUR", want: MappingPriceMismatch, message: "costs 49.00 EUR; the price is 50.00 EUR excluding tax"},
		{name: "trial missing on the free-month tier", v: with(func(v *Variant) { v.HasFreeTrial = false }), currency: "EUR", freeTier: true, want: MappingTrialMissing, message: "Pro is the free-month tier"},
		{name: "no trial needed elsewhere", v: with(func(v *Variant) { v.HasFreeTrial = false }), currency: "EUR", want: MappingOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := CheckMapping(price, tc.v, tc.currency, true, tc.freeTier)
			if got != tc.want || !strings.Contains(msg, tc.message) || (got == MappingOK) != (msg == "") {
				t.Fatalf("CheckMapping = %s, %q", got, msg)
			}
		})
	}
}

func TestFormatMinor(t *testing.T) {
	for in, want := range map[int64]string{5000: "50.00 EUR", 5: "0.05 EUR", -150: "-1.50 EUR"} {
		if got := FormatMinor(in, "eur"); got != want {
			t.Fatalf("FormatMinor(%d) = %q, want %q", in, got, want)
		}
	}
	if got := FormatMinor(500, "JPY"); got != "500 JPY" {
		t.Fatalf("zero-decimal = %q", got)
	}
}

func TestNewVariantMap(t *testing.T) {
	m, err := NewVariantMap(map[string]string{"a": "1", "b": "2"})
	if err != nil || m.Len() != 2 {
		t.Fatalf("NewVariantMap: %v %d", err, m.Len())
	}
	if p, ok := m.Price("2"); !ok || p != "b" {
		t.Fatalf("Price(2) = %q %v", p, ok)
	}
	if _, err := NewVariantMap(map[string]string{"a": "1", "b": "1"}); err == nil {
		t.Fatal("variant selling two prices accepted")
	}
}
