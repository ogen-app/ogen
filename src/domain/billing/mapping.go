package billing

import (
	"fmt"
	"strconv"
	"strings"
)

// Product mapping statuses: the result of checking a price row against the
// product that sells it.
const (
	MappingOK       = "ok"
	MappingUnmapped = "unmapped"
	// MappingProductMissing: the product is not a recurring product of the
	// mode's organization.
	MappingProductMissing  = "product_missing"
	MappingProductArchived = "product_archived"
	MappingPriceMismatch   = "price_mismatch"
	// MappingCurrencyMismatch: the product has no fixed price in the price
	// row's currency.
	MappingCurrencyMismatch = "currency_mismatch"
	MappingIntervalMismatch = "interval_mismatch"
	MappingModeMismatch     = "mode_mismatch"
	MappingTrialMissing     = "trial_missing"
	// MappingUnchecked: the mode's catalog could not be read (no token for
	// it, or the provider is unreachable).
	MappingUnchecked = "unchecked"
	// MappingProductTaken refuses a save only: the product already sells
	// another price in that currency and mode.
	MappingProductTaken = "product_taken"
)

// CheckMapping compares a product with the price row it sells, in the given
// mode. The product must be on sale, bill the row's amount excluding tax in
// the row's currency, every one of its intervals, in that mode; a product of
// the free-month tier must also carry a one-month trial. It returns MappingOK
// and "", or a status and a message an operator can act on.
func CheckMapping(p Price, prod Product, testMode, freeTrialTier bool) (status, message string) {
	amount, priced := prod.PriceIn(p.Currency)
	switch {
	case prod.Archived:
		return MappingProductArchived, fmt.Sprintf("Product %s is archived; unarchive it or map another product.", prod.ID)
	case prod.TestMode != testMode:
		return MappingModeMismatch, fmt.Sprintf("Product %s is a %s product; this is the %s map.", prod.ID, modeName(prod.TestMode), modeName(testMode))
	case prod.Interval != p.Interval || max(prod.IntervalCount, 1) != 1:
		return MappingIntervalMismatch, fmt.Sprintf("Product %s bills %s; the price is billed every %s.", prod.ID, every(prod.Interval, prod.IntervalCount), p.Interval)
	case !priced:
		return MappingCurrencyMismatch, fmt.Sprintf("Product %s has no fixed price in %s.", prod.ID, strings.ToUpper(p.Currency))
	case amount != p.NetMinor:
		return MappingPriceMismatch, fmt.Sprintf("Product %s costs %s; the price is %s excluding tax.", prod.ID, FormatMinor(amount, p.Currency), FormatMinor(p.NetMinor, p.Currency))
	case freeTrialTier && !prod.OneMonthTrial():
		return MappingTrialMissing, fmt.Sprintf("%s is the free-month tier, so product %s needs a 1-month free trial.", p.TierName, prod.ID)
	}
	return MappingOK, ""
}

// zeroDecimal are the currencies whose minor unit is the major unit.
var zeroDecimal = map[string]bool{"JPY": true, "KRW": true, "VND": true, "CLP": true, "ISK": true, "UGX": true}

// MinorDigits is how many decimal places a currency's minor unit has.
func MinorDigits(currency string) int {
	if zeroDecimal[strings.ToUpper(currency)] {
		return 0
	}
	return 2
}

// FormatMinor renders an amount in minor units with its currency, e.g.
// "50.00 EUR". Zero-decimal currencies print whole units.
func FormatMinor(minor int64, currency string) string {
	currency = strings.ToUpper(currency)
	if MinorDigits(currency) == 0 {
		return strconv.FormatInt(minor, 10) + " " + currency
	}
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, minor/100, minor%100, currency)
}

func modeName(test bool) string {
	if test {
		return "sandbox"
	}
	return "production"
}

func every(interval string, count int) string {
	switch {
	case interval == "":
		return "once (not a subscription)"
	case count > 1:
		return fmt.Sprintf("every %d %ss", count, interval)
	}
	return "every " + interval
}
