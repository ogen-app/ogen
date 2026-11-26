package billing

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// USD conversion of payments in other currencies. The European Central Bank
// publishes one reference rate per currency per business day, quoted against
// the euro; a USD amount goes through the euro (amount ÷ the currency's rate ×
// the dollar's rate). Each payment uses the rate of its UTC day, or of the
// latest business day before it.

// FX sources: how a payment's USD amounts were obtained.
const (
	FXSourceIdentity = "identity" // a USD payment
	FXSourceECB      = "ecb"
	FXSourcePolar    = "polar"
)

// ErrNoRate is returned when no rate covers a payment's day yet.
var ErrNoRate = errors.New("billing: no exchange rate for that day yet")

// FXRate is the units of Currency one euro buys on Date.
type FXRate struct {
	Date     time.Time
	Currency string
	PerEUR   *big.Rat
}

// FXTable answers rate lookups from a set of ECB rates.
type FXTable struct {
	// byCurrency holds each currency's rates, oldest first.
	byCurrency map[string][]FXRate
	// latest is the newest day any rate is known for.
	latest time.Time
}

// NewFXTable indexes rates. They may come in any order.
func NewFXTable(rates []FXRate) FXTable {
	t := FXTable{byCurrency: map[string][]FXRate{}}
	for _, r := range rates {
		r.Currency, r.Date = strings.ToUpper(r.Currency), day(r.Date)
		t.byCurrency[r.Currency] = append(t.byCurrency[r.Currency], r)
		if r.Date.After(t.latest) {
			t.latest = r.Date
		}
	}
	for _, rs := range t.byCurrency {
		slices.SortFunc(rs, func(a, b FXRate) int { return a.Date.Compare(b.Date) })
	}
	return t
}

// Conversion is how one payment's amounts become US cents.
type Conversion struct {
	// Rate is USD per one unit of the payment's currency.
	Rate   *big.Rat
	Source string
	// Date is the day of the rate; zero for a USD payment.
	Date     time.Time
	currency string
}

// IdentityConversion converts a USD payment.
func IdentityConversion() Conversion {
	return Conversion{Rate: big.NewRat(1, 1), Source: FXSourceIdentity, currency: "USD"}
}

// ToUSD finds the conversion of a payment in currency made at a time. A USD
// payment converts at 1. Otherwise the rate is the one of the payment's UTC
// day, or of the latest day before it when that day has none (a weekend or a
// holiday), and only once a rate of that day or later is known, so the result
// does not depend on when the lookup runs. ErrNoRate otherwise.
func (t FXTable) ToUSD(currency string, at time.Time) (Conversion, error) {
	currency = strings.ToUpper(currency)
	if currency == "USD" {
		return IdentityConversion(), nil
	}
	d := day(at)
	if t.latest.Before(d) {
		return Conversion{}, ErrNoRate
	}
	usd, okUSD := t.on("USD", d)
	var per *big.Rat
	ok := true
	if currency == "EUR" {
		per = big.NewRat(1, 1)
	} else {
		var r FXRate
		r, ok = t.on(currency, d)
		per = r.PerEUR
	}
	if !okUSD || !ok || per.Sign() <= 0 {
		return Conversion{}, ErrNoRate
	}
	return Conversion{Rate: new(big.Rat).Quo(usd.PerEUR, per), Source: FXSourceECB, Date: usd.Date, currency: currency}, nil
}

// on returns a currency's rate of a day, or of the latest day before it.
func (t FXTable) on(currency string, d time.Time) (FXRate, bool) {
	for _, r := range slices.Backward(t.byCurrency[currency]) {
		if !r.Date.After(d) {
			return r, true
		}
	}
	return FXRate{}, false
}

// Cents converts an amount in minor units of the conversion's currency to US
// cents, rounding half to even once.
func (c Conversion) Cents(minor int64) int64 {
	// minor / 10^digits × rate × 100
	v := new(big.Rat).Mul(big.NewRat(minor, 1), c.Rate)
	v.Mul(v, big.NewRat(100, pow10(MinorDigits(c.currency))))
	return roundHalfEven(v)
}

// RateString renders the rate with ten decimal places, as it is stored.
func (c Conversion) RateString() string {
	return strings.TrimRight(strings.TrimRight(c.Rate.FloatString(10), "0"), ".")
}

// ParseRate reads a decimal rate such as "1.0512".
func ParseRate(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("billing: bad exchange rate %q", s)
	}
	return r, nil
}

func roundHalfEven(v *big.Rat) int64 {
	num, den := new(big.Int).Set(v.Num()), v.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	switch new(big.Int).Mul(r, big.NewInt(2)).Cmp(den) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	if neg {
		q.Neg(q)
	}
	return q.Int64()
}

func pow10(n int) int64 {
	out := int64(1)
	for range n {
		out *= 10
	}
	return out
}

func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
