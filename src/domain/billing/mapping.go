package billing

import (
	"fmt"
	"strconv"
	"strings"
)

// Variant mapping statuses: the result of checking a price row against the
// variant that sells it.
const (
	MappingOK               = "ok"
	MappingUnmapped         = "unmapped"
	MappingVariantMissing   = "variant_missing"
	MappingPriceMismatch    = "price_mismatch"
	MappingCurrencyMismatch = "currency_mismatch"
	MappingIntervalMismatch = "interval_mismatch"
	MappingModeMismatch     = "mode_mismatch"
	MappingTrialMissing     = "trial_missing"
	// MappingUnchecked: the mode's catalog could not be read (no API key for
	// it, or the provider is unreachable).
	MappingUnchecked = "unchecked"
	// MappingVariantTaken refuses a save only: the variant already sells
	// another price in the mode.
	MappingVariantTaken = "variant_taken"
)

// VariantPublished is the provider status of a variant that can be bought.
const VariantPublished = "published"

// CheckMapping compares a variant with the price row it sells, in the store's
// currency and the given mode. The variant must bill the row's amount
// excluding tax, in its currency, every one of its intervals, and in that
// mode; a variant of the free-month tier must also carry a one-month trial.
// It returns MappingOK and "", or a status and a message an operator can act
// on.
func CheckMapping(p Price, v Variant, currency string, testMode, freeTrialTier bool) (status, message string) {
	switch {
	case v.Status != "" && v.Status != VariantPublished:
		return MappingVariantMissing, fmt.Sprintf("Variant %s is %s in Lemon Squeezy; publish it first.", v.ID, v.Status)
	case v.TestMode != testMode:
		return MappingModeMismatch, fmt.Sprintf("Variant %s is a %s variant; this is the %s map.", v.ID, modeName(v.TestMode), modeName(testMode))
	case v.Interval != p.Interval || max(v.IntervalCount, 1) != 1:
		return MappingIntervalMismatch, fmt.Sprintf("Variant %s bills %s; the price is billed every %s.", v.ID, every(v.Interval, v.IntervalCount), p.Interval)
	case !strings.EqualFold(currency, p.Currency):
		return MappingCurrencyMismatch, fmt.Sprintf("The store sells in %s; the price is in %s.", strings.ToUpper(currency), strings.ToUpper(p.Currency))
	case v.PriceMinor != p.NetMinor:
		return MappingPriceMismatch, fmt.Sprintf("Variant %s costs %s; the price is %s excluding tax.", v.ID, FormatMinor(v.PriceMinor, currency), FormatMinor(p.NetMinor, p.Currency))
	case freeTrialTier && !v.OneMonthTrial():
		return MappingTrialMissing, fmt.Sprintf("%s is the free-month tier, so variant %s needs a 1-month free trial.", p.TierName, v.ID)
	}
	return MappingOK, ""
}

// FormatMinor renders an amount in minor units with its currency, e.g.
// "50.00 EUR". Zero-decimal currencies print whole units.
func FormatMinor(minor int64, currency string) string {
	currency = strings.ToUpper(currency)
	switch currency {
	case "JPY", "KRW", "VND", "CLP", "ISK", "UGX":
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
		return "test-mode"
	}
	return "live"
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
