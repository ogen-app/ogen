package billing

import "testing"

func TestFeeFormulaEstimate(t *testing.T) {
	f := FeeFormula{PercentBPS: 500, FixedUSDCents: 50}
	cases := []struct {
		name        string
		p           Payment
		fee, feeUSD int64
	}{
		{"usd invoice", Payment{Kind: PaymentKindSubscriptionInvoice, Total: 2900, TotalUSD: 2900}, 195, 195},
		{"eur order", Payment{Kind: PaymentKindOrder, Total: 1210, TotalUSD: 1307}, 106, 115},
		{"free trial", Payment{Kind: PaymentKindOrder}, 0, 0},
		{"refund", Payment{Kind: PaymentKindRefund, Total: 605, TotalUSD: 654}, 0, 0},
	}
	for _, c := range cases {
		fee, feeUSD := f.Estimate(c.p)
		if fee != c.fee || feeUSD != c.feeUSD {
			t.Errorf("%s: fee = %d / %d USD, want %d / %d", c.name, fee, feeUSD, c.fee, c.feeUSD)
		}
	}
}

func TestShare(t *testing.T) {
	if got := Share(210, 605, 1210); got != 105 {
		t.Errorf("Share = %d, want 105", got)
	}
	if got := Share(227, 654, 1307); got != 114 {
		t.Errorf("Share = %d, want 114", got)
	}
	if got := Share(10, 5, 0); got != 0 {
		t.Errorf("Share over zero = %d, want 0", got)
	}
}
