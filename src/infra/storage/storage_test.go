package storage

import (
	"testing"

	"github.com/ogen-app/ogen/src/kernel/config"
)

// TestDeletePrefixRejectsNonFolder: a prefix that is empty or not a folder
// would match far more than one asset's or post's objects, so it is refused
// before any request is made.
func TestDeletePrefixRejectsNonFolder(t *testing.T) {
	s, err := New(&config.Config{StorageEndpoint: "http://127.0.0.1:1", StorageRegion: "auto", StorageBucket: "b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, prefix := range []string{"", "/", "//", "assets", "t/x/assets/a1"} {
		if err := s.DeletePrefix(t.Context(), prefix); err == nil {
			t.Errorf("DeletePrefix(%q) = nil, want a refusal", prefix)
		}
	}
}
