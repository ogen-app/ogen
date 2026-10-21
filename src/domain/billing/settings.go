package billing

import (
	"fmt"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ProviderNone turns billing off.
const ProviderNone = "none"

// Settings is the non-secret billing configuration.
type Settings struct {
	// Provider is none | lemonsqueezy.
	Provider string
	TestMode bool
	StoreID  string
	Variants VariantMap
	// TrialTeardownLive tears down never-paid trials; false only reports them.
	TrialTeardownLive bool
	// AppBaseURL builds the checkout return link.
	AppBaseURL string
}

// Enabled reports whether billing is on.
func (s Settings) Enabled() bool {
	return s.Provider != "" && s.Provider != ProviderNone
}

// ParseSettings validates the env values into Settings.
func ParseSettings(provider string, testMode bool, storeID, variants string, teardownLive bool, appBaseURL string) (Settings, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = ProviderNone
	}
	s := Settings{Provider: provider, TestMode: testMode, StoreID: strings.TrimSpace(storeID), TrialTeardownLive: teardownLive, AppBaseURL: strings.TrimRight(appBaseURL, "/")}
	switch provider {
	case ProviderNone:
		return s, nil
	case models.BillingProviderLemonSqueezy:
	default:
		return Settings{}, fmt.Errorf("billing: unknown BILLING_PROVIDER %q (want none or lemonsqueezy)", provider)
	}
	if s.StoreID == "" {
		return Settings{}, fmt.Errorf("billing: LEMONSQUEEZY_STORE_ID is required when BILLING_PROVIDER=lemonsqueezy")
	}
	m, err := ParseVariantMap(variants)
	if err != nil {
		return Settings{}, err
	}
	s.Variants = m
	return s, nil
}
