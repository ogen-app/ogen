package zernio

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TestCatalogResolutionVsAvailability locks in the CON-292 §11 split: resolution
// lookups (publish path) see disabled rows so already-scheduled posts still go
// out, while availability surfaces (connect / composer) hide them.
func TestCatalogResolutionVsAvailability(t *testing.T) {
	// Restore the empty snapshot so this white-box mutation can't leak into
	// other tests in the package.
	defer activeCatalog.Store(newCatalog(nil))

	rows := []models.Platform{
		{ID: "sqidX", Name: "X", ZernioID: "twitter", Enabled: true, SupportedPostTypes: models.StringSlice{"text-post", "thread"}},
		{ID: "sqidTT", Name: "TikTok", ZernioID: "tiktok", Enabled: false, SupportedPostTypes: models.StringSlice{"video"}},
		{ID: "sqidDraft", Name: "Draft", ZernioID: "", Enabled: true},
	}
	activeCatalog.Store(newCatalog(rows))

	// Resolution sees disabled rows (publish path keeps working on disable).
	if sp := LookupSupportedBySqid("sqidTT"); sp == nil || sp.ZernioID != "tiktok" {
		t.Fatalf("disabled platform must resolve by sqid for the publish path, got %+v", sp)
	}
	if got := LookupSqidByZernioID("tiktok"); got != "sqidTT" {
		t.Fatalf("LookupSqidByZernioID(tiktok) = %q, want sqidTT", got)
	}

	// A row without an assigned slug is not resolvable.
	if sp := LookupSupportedBySqid("sqidDraft"); sp != nil {
		t.Fatalf("slugless row must not resolve, got %+v", sp)
	}

	// Availability hides disabled / unknown platforms.
	if sp := LookupSupportedPlatform("tiktok"); sp != nil {
		t.Fatalf("disabled platform must be hidden from connect/availability, got %+v", sp)
	}
	if sp := LookupSupportedPlatform("twitter"); sp == nil {
		t.Fatalf("enabled platform must be available for connect")
	}
	enabled := SupportedPlatforms()
	if len(enabled) != 1 || enabled[0].ZernioID != "twitter" {
		t.Fatalf("SupportedPlatforms must return only enabled, got %+v", enabled)
	}

	// Returned entries are copies — a caller mutating them must not corrupt the
	// shared snapshot.
	enabled[0].Label = "mutated"
	if again := SupportedPlatforms(); again[0].Label != "X" {
		t.Fatalf("SupportedPlatforms must return copies; snapshot was corrupted: %+v", again)
	}
}
