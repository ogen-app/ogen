package billing

import (
	"strings"
	"testing"
)

func TestCheckMapping(t *testing.T) {
	price := Price{ID: "p", TierName: "Pro", Currency: "EUR", Interval: "month", NetMinor: 5000}
	good := Product{ID: "prod_1", Interval: "month", IntervalCount: 1, TestMode: true, HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1,
		Prices: []ProductPrice{{Currency: "USD", AmountMinor: 5500}, {Currency: "EUR", AmountMinor: 5000}}}
	with := func(f func(*Product)) Product { p := good; f(&p); return p }

	for _, tc := range []struct {
		name     string
		p        Product
		freeTier bool
		want     string
		message  string
	}{
		{name: "match", p: good, freeTier: true, want: MappingOK},
		{name: "archived", p: with(func(p *Product) { p.Archived = true }), want: MappingProductArchived, message: "is archived"},
		{name: "count zero reads as one", p: with(func(p *Product) { p.IntervalCount = 0 }), want: MappingOK},
		{name: "other mode", p: with(func(p *Product) { p.TestMode = false }), want: MappingModeMismatch, message: "production product"},
		{name: "yearly", p: with(func(p *Product) { p.Interval = "year" }), want: MappingIntervalMismatch, message: "every year"},
		{name: "quarterly", p: with(func(p *Product) { p.IntervalCount = 3 }), want: MappingIntervalMismatch, message: "every 3 months"},
		{name: "one-off", p: with(func(p *Product) { p.Interval = "" }), want: MappingIntervalMismatch, message: "not a subscription"},
		{name: "no price in the currency", p: with(func(p *Product) { p.Prices = p.Prices[:1] }), want: MappingCurrencyMismatch, message: "no fixed price in EUR"},
		{name: "amount", p: with(func(p *Product) { p.Prices = []ProductPrice{{Currency: "eur", AmountMinor: 4900}} }), want: MappingPriceMismatch, message: "costs 49.00 EUR; the price is 50.00 EUR excluding tax"},
		{name: "trial missing on the free-month tier", p: with(func(p *Product) { p.HasFreeTrial = false }), freeTier: true, want: MappingTrialMissing, message: "Pro is the free-month tier"},
		{name: "no trial needed elsewhere", p: with(func(p *Product) { p.HasFreeTrial = false }), want: MappingOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := CheckMapping(price, tc.p, true, tc.freeTier)
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

func TestProductMap(t *testing.T) {
	m, err := NewProductMap([]ProductMapping{
		{PriceID: "pro-eur", ProductID: "prod_pro", Currency: "eur"},
		{PriceID: "pro-usd", ProductID: "prod_pro", Currency: "USD"},
		{PriceID: "max-eur", ProductID: "prod_max", Currency: "EUR"},
	})
	if err != nil || m.Len() != 3 {
		t.Fatalf("NewProductMap: %v %d", err, m.Len())
	}
	if p, ok := m.Price("prod_pro", "usd"); !ok || p != "pro-usd" {
		t.Fatalf("Price(prod_pro, usd) = %q %v", p, ok)
	}
	if _, ok := m.Price("prod_pro", ""); ok {
		t.Fatal("a product selling two prices resolved without a currency")
	}
	if p, ok := m.Price("prod_max", ""); !ok || p != "max-eur" {
		t.Fatalf("a product selling one price resolves without a currency: %q %v", p, ok)
	}
	if prod, ok := m.Product("pro-eur"); !ok || prod != "prod_pro" {
		t.Fatalf("Product(pro-eur) = %q %v", prod, ok)
	}
	if _, err := NewProductMap([]ProductMapping{{PriceID: "a", ProductID: "x", Currency: "EUR"}, {PriceID: "b", ProductID: "x", Currency: "eur"}}); err == nil {
		t.Fatal("a product selling two prices in one currency accepted")
	}
}

func TestProductTrial(t *testing.T) {
	p := Product{HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1}
	if !p.OneMonthTrial() || p.TrialDays() != 30 {
		t.Fatalf("one-month trial: %v %d", p.OneMonthTrial(), p.TrialDays())
	}
	p.TrialInterval, p.TrialIntervalCount = "day", 14
	if p.OneMonthTrial() || p.TrialDays() != 14 {
		t.Fatalf("14-day trial: %v %d", p.OneMonthTrial(), p.TrialDays())
	}
}
