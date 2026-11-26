package billing

import "testing"

func TestParseSettings(t *testing.T) {
	s, err := ParseSettings(SettingsInput{Provider: " Polar ", TestMode: true, OrganizationID: "org_1", AppBaseURL: "https://app.example/"})
	if err != nil || !s.Enabled() || s.Provider != "polar" || s.APIVersion != DefaultPolarAPIVersion || s.AppBaseURL != "https://app.example" {
		t.Fatalf("polar: %+v %v", s, err)
	}
	if _, err := ParseSettings(SettingsInput{Provider: "polar"}); err == nil {
		t.Fatal("polar without an organization accepted")
	}
	if s, err := ParseSettings(SettingsInput{}); err != nil || s.Enabled() {
		t.Fatalf("empty: %+v %v", s, err)
	}
	// The provider this build dropped turns billing off instead of failing
	// boot.
	s, err = ParseSettings(SettingsInput{Provider: "lemonsqueezy"})
	if err != nil || s.Enabled() || s.LegacyProvider != "lemonsqueezy" {
		t.Fatalf("lemonsqueezy: %+v %v", s, err)
	}
	if _, err := ParseSettings(SettingsInput{Provider: "stripe"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
