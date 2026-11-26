package billing

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ProviderNone turns billing off.
const ProviderNone = "none"

// DefaultPolarAPIVersion is the Polar API version requests and the webhook
// endpoint are pinned to unless POLAR_API_VERSION says otherwise.
const DefaultPolarAPIVersion = "2026-10"

// Settings is the non-secret billing configuration.
type Settings struct {
	// Provider is none | polar.
	Provider string
	// LegacyProvider is a BILLING_PROVIDER value this build no longer
	// serves (lemonsqueezy). Billing is then off, and boot reports it.
	LegacyProvider string
	// TestMode runs against the provider's sandbox.
	TestMode bool
	// OrganizationID is the provider organization of the running mode;
	// OrganizationIDLive the production one, read while still in sandbox.
	OrganizationID     string
	OrganizationIDLive string
	APIVersion         string
	// TrialTeardownLive tears down never-paid trials; false only reports them.
	TrialTeardownLive bool
	// AppBaseURL builds the checkout return link.
	AppBaseURL string
}

// SettingsInput is the billing env.
type SettingsInput struct {
	Provider           string
	TestMode           bool
	OrganizationID     string
	OrganizationIDLive string
	APIVersion         string
	TrialTeardownLive  bool
	AppBaseURL         string
}

// Enabled reports whether billing is on.
func (s Settings) Enabled() bool {
	return s.Provider != "" && s.Provider != ProviderNone
}

// ParseSettings validates the env values into Settings. A provider this
// build dropped turns billing off instead of failing boot, so the image can
// roll out before the env names the new provider.
func ParseSettings(in SettingsInput) (Settings, error) {
	provider := cmp.Or(strings.ToLower(strings.TrimSpace(in.Provider)), ProviderNone)
	s := Settings{
		Provider: provider, TestMode: in.TestMode,
		OrganizationID: strings.TrimSpace(in.OrganizationID), OrganizationIDLive: strings.TrimSpace(in.OrganizationIDLive),
		APIVersion:        cmp.Or(strings.TrimSpace(in.APIVersion), DefaultPolarAPIVersion),
		TrialTeardownLive: in.TrialTeardownLive, AppBaseURL: strings.TrimRight(in.AppBaseURL, "/"),
	}
	switch provider {
	case ProviderNone:
		return s, nil
	case models.BillingProviderLemonSqueezy:
		s.Provider, s.LegacyProvider = ProviderNone, provider
		return s, nil
	case models.BillingProviderPolar:
	default:
		return Settings{}, fmt.Errorf("billing: unknown BILLING_PROVIDER %q (want none or polar)", provider)
	}
	if s.OrganizationID == "" {
		return Settings{}, fmt.Errorf("billing: POLAR_ORGANIZATION_ID is required when BILLING_PROVIDER=polar")
	}
	return s, nil
}
