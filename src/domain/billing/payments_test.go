package billing

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestFeeFormulaEstimate(t *testing.T) {
	f := FeeFormula{PercentBPS: 500, FixedUSDCents: 50}
	cases := []struct {
		name            string
		kind            string
		total, totalUSD int64
		fee, feeUSD     int64
	}{
		{"usd order", PaymentKindOrder, 2900, 2900, 195, 195},
		{"eur order", PaymentKindOrder, 1210, 1307, 106, 115},
		{"free trial", PaymentKindOrder, 0, 0, 0, 0},
		{"refund", PaymentKindRefund, 605, 654, 0, 0},
	}
	for _, c := range cases {
		fee, feeUSD := f.Estimate(c.kind, c.total, c.totalUSD)
		if fee != c.fee || feeUSD != c.feeUSD {
			t.Errorf("%s: fee = %d / %d USD, want %d / %d", c.name, fee, feeUSD, c.fee, c.feeUSD)
		}
	}
}

func TestShare(t *testing.T) {
	if got := Share(210, 605, 1210); got != 105 {
		t.Errorf("Share = %d, want 105", got)
	}
	if got := Share(10, 5, 0); got != 0 {
		t.Errorf("Share over zero = %d, want 0", got)
	}
}

func rate(s string) *big.Rat {
	r, err := ParseRate(s)
	if err != nil {
		panic(err)
	}
	return r
}

func TestFXTableToUSD(t *testing.T) {
	fri := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	mon := fri.AddDate(0, 0, 3)
	table := NewFXTable([]FXRate{
		{Date: mon, Currency: "USD", PerEUR: rate("1.1000")},
		{Date: fri, Currency: "USD", PerEUR: rate("1.0500")},
		{Date: fri, Currency: "gbp", PerEUR: rate("0.8400")},
		{Date: fri, Currency: "JPY", PerEUR: rate("160")},
	})

	if c, err := table.ToUSD("usd", time.Now()); err != nil || c.Source != FXSourceIdentity || c.Cents(1234) != 1234 || c.RateString() != "1" {
		t.Fatalf("USD: %+v %v", c, err)
	}
	// A Saturday payment uses Friday's rate once a later day is known.
	sat := fri.AddDate(0, 0, 1).Add(15 * time.Hour)
	c, err := table.ToUSD("EUR", sat)
	if err != nil || c.Source != FXSourceECB || !c.Date.Equal(fri) || c.Cents(1000) != 1050 || c.RateString() != "1.05" {
		t.Fatalf("EUR on Saturday: %+v %v", c, err)
	}
	// 1000 GBP pence at 1.05/0.84 = 1250 cents.
	if c, err := table.ToUSD("GBP", sat); err != nil || c.Cents(1000) != 1250 {
		t.Fatalf("GBP: %+v %v", c, err)
	}
	// Zero-decimal: 1600 JPY at 1.05/160 = 10.50 USD.
	if c, err := table.ToUSD("JPY", sat); err != nil || c.Cents(1600) != 1050 {
		t.Fatalf("JPY: %d %v", c.Cents(1600), err)
	}
	// After the latest known day, the conversion waits for that day's rate.
	if _, err := table.ToUSD("EUR", mon.AddDate(0, 0, 1)); !errors.Is(err, ErrNoRate) {
		t.Fatalf("future day = %v, want ErrNoRate", err)
	}
	// A currency the ECB does not quote has no rate.
	if _, err := table.ToUSD("XYZ", sat); !errors.Is(err, ErrNoRate) {
		t.Fatalf("unknown currency = %v, want ErrNoRate", err)
	}
}

func TestConversionRoundsHalfEven(t *testing.T) {
	c := Conversion{Rate: big.NewRat(1, 2), currency: "EUR"}
	for in, want := range map[int64]int64{1: 0, 3: 2, 5: 2, 7: 4, -3: -2} {
		if got := c.Cents(in); got != want {
			t.Errorf("Cents(%d) at 0.5 = %d, want %d", in, got, want)
		}
	}
}
