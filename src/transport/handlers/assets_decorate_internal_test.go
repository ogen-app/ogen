package handlers

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
)

// decorateStubStorage satisfies storage.Storage by embedding it (left nil) and
// overriding only PublicURL — the single method decorateFile calls. Any other
// method would panic on the nil embed, which is exactly the guard we want: the
// test fails loudly if decorateFile ever reaches for more than a key→URL mint.
type decorateStubStorage struct{ storage.Storage }

func (decorateStubStorage) PublicURL(key string) string { return "https://pub.example.com/" + key }

// TestDecorateFile_MintsNormalizedURL covers CON-299: decorateFile turns the
// stored original / thumbnail / normalized keys into public URLs. The normalized
// copy is the browser-drawable one shown for HEIC/TIFF; its absence (a failed or
// pending extraction has no key) yields no normalized_url, which the client reads
// as "nothing to show" — AC3 (minted per response) and AC4 (absence tolerated).
func TestDecorateFile_MintsNormalizedURL(t *testing.T) {
	h := &AssetsHandler{storage: decorateStubStorage{}}
	const base = "https://pub.example.com/"

	t.Run("all three keys mint their URL", func(t *testing.T) {
		normKey := "t/x/assets/i1/normalized.png"
		a := &models.Asset{File: &models.AssetFile{
			S3Key:           "t/x/assets/i1/original.heic",
			ThumbnailS3Key:  &normKey,
			NormalizedS3Key: &normKey,
		}}
		h.decorateFile(a)
		if a.File.URL == nil || *a.File.URL != base+"t/x/assets/i1/original.heic" {
			t.Fatalf("url = %v", a.File.URL)
		}
		if a.File.NormalizedURL == nil || *a.File.NormalizedURL != base+normKey {
			t.Fatalf("normalized_url = %v, want minted from the normalized key", a.File.NormalizedURL)
		}
		if a.File.ThumbnailURL == nil || *a.File.ThumbnailURL != base+normKey {
			t.Fatalf("thumbnail_url = %v", a.File.ThumbnailURL)
		}
	})

	t.Run("no normalized key => no normalized_url", func(t *testing.T) {
		a := &models.Asset{File: &models.AssetFile{S3Key: "t/x/assets/i2/original.png"}}
		h.decorateFile(a)
		if a.File.URL == nil {
			t.Fatal("url should still be minted from the original key")
		}
		if a.File.NormalizedURL != nil {
			t.Fatalf("normalized_url should be nil when there is no key, got %q", *a.File.NormalizedURL)
		}
	})
}
