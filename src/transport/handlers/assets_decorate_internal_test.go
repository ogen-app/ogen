package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
)

const (
	pubBase  = "https://pub.example.com/"
	signBase = "https://s3.example.com/bucket/"
)

// decorateStubStorage satisfies storage.Storage by embedding it (left nil) and
// overriding only the two URL minters decorateFile may reach for. Any other
// method would panic on the nil embed, which is exactly the guard we want: the
// test fails loudly if decoration ever does more than mint a key→URL — no
// object reads, no network.
type decorateStubStorage struct {
	storage.Storage
	failKey string // PresignedGetURL fails for this key
}

func (decorateStubStorage) PublicURL(key string) string { return pubBase + key }

func (s decorateStubStorage) PresignedGetURL(_ context.Context, key string, _ time.Duration) (string, error) {
	if key == s.failKey {
		return "", errors.New("presign failed")
	}
	return signBase + key + "?X-Amz-Signature=abc&X-Amz-Expires=900", nil
}

// TestDecorateFile_MintsNormalizedURL covers CON-299: decorateFile turns the
// stored original / thumbnail / normalized keys into URLs. The normalized copy is
// the browser-drawable one shown for HEIC/TIFF; its absence (a failed or pending
// extraction has no key) yields no normalized_url, which the client reads as
// "nothing to show".
func TestDecorateFile_MintsNormalizedURL(t *testing.T) {
	h := &AssetsHandler{storage: decorateStubStorage{}}

	t.Run("all three keys mint their URL", func(t *testing.T) {
		normKey := "t/x/assets/i1/normalized.png"
		a := &models.Asset{File: &models.AssetFile{
			S3Key:           "t/x/assets/i1/original.heic",
			ThumbnailS3Key:  &normKey,
			NormalizedS3Key: &normKey,
		}}
		h.decorateFile(t.Context(), a)
		if a.File.URL == nil || !strings.HasPrefix(*a.File.URL, signBase+"t/x/assets/i1/original.heic?") {
			t.Fatalf("url = %v", a.File.URL)
		}
		if a.File.NormalizedURL == nil || !strings.HasPrefix(*a.File.NormalizedURL, signBase+normKey+"?") {
			t.Fatalf("normalized_url = %v, want minted from the normalized key", a.File.NormalizedURL)
		}
		if a.File.ThumbnailURL == nil || !strings.HasPrefix(*a.File.ThumbnailURL, signBase+normKey+"?") {
			t.Fatalf("thumbnail_url = %v", a.File.ThumbnailURL)
		}
	})

	t.Run("no normalized key => no normalized_url", func(t *testing.T) {
		a := &models.Asset{File: &models.AssetFile{S3Key: "t/x/assets/i2/original.png"}}
		h.decorateFile(t.Context(), a)
		if a.File.URL == nil {
			t.Fatal("url should still be minted from the original key")
		}
		if a.File.NormalizedURL != nil {
			t.Fatalf("normalized_url should be nil when there is no key, got %q", *a.File.NormalizedURL)
		}
	})
}

// TestDecorateFile_SignsByDefault: asset file URLs are presigned
// unless the deployment opted in to public reads, and a key that fails to sign
// leaves its URL unset rather than falling back to a public URL a private bucket
// refuses.
func TestDecorateFile_SignsByDefault(t *testing.T) {
	thumb := "t/x/assets/p1/thumbnail.png"
	newAsset := func() *models.Asset {
		return &models.Asset{File: &models.AssetFile{S3Key: "t/x/assets/p1/original.pdf", ThumbnailS3Key: &thumb}}
	}

	t.Run("public read opt-in serves public URLs", func(t *testing.T) {
		h := &AssetsHandler{storage: decorateStubStorage{}, publicRead: true}
		a := newAsset()
		h.decorateFile(t.Context(), a)
		if a.File.URL == nil || *a.File.URL != pubBase+"t/x/assets/p1/original.pdf" {
			t.Fatalf("url = %v, want public", a.File.URL)
		}
		if a.File.ThumbnailURL == nil || *a.File.ThumbnailURL != pubBase+thumb {
			t.Fatalf("thumbnail_url = %v, want public", a.File.ThumbnailURL)
		}
	})

	t.Run("a failed signature leaves only that URL unset", func(t *testing.T) {
		h := &AssetsHandler{storage: decorateStubStorage{failKey: thumb}}
		a := newAsset()
		h.decorateFile(t.Context(), a)
		if a.File.ThumbnailURL != nil {
			t.Fatalf("thumbnail_url = %q, want unset on a presign failure", *a.File.ThumbnailURL)
		}
		if a.File.URL == nil || !strings.HasPrefix(*a.File.URL, signBase) {
			t.Fatalf("url = %v, want signed", a.File.URL)
		}
	})
}

// TestAttachImages_RelinksScrapedMarkdown: a scraped page's
// Markdown stores its mirrored images as public URLs, so a read points them at
// the signed image URLs, and the write path maps an echoed signed link back.
func TestAttachImages_RelinksScrapedMarkdown(t *testing.T) {
	const k0, k1 = "t/x/assets/u1/images/0.png", "t/x/assets/u1/images/1.jpg"
	stored := "# Page\n\n![hero](" + pubBase + k0 + ")\n\ntext ![](" + pubBase + k1 + " \"t\")\n\n![ext](https://other.example.com/a.png)\n"
	images := func() []models.AssetImage {
		return []models.AssetImage{{S3Key: k0}, {S3Key: k1}}
	}

	t.Run("read swaps stored links for signed ones", func(t *testing.T) {
		h := &AssetsHandler{storage: decorateStubStorage{}}
		a := &models.Asset{Content: stored}
		h.attachImages(t.Context(), a, images())
		if a.Images[0].URL == nil || !strings.HasPrefix(*a.Images[0].URL, signBase+k0+"?") {
			t.Fatalf("images[0].url = %v", a.Images[0].URL)
		}
		if strings.Contains(a.Content, pubBase) {
			t.Fatalf("content still links public URLs:\n%s", a.Content)
		}
		for _, img := range a.Images {
			if !strings.Contains(a.Content, "("+*img.URL) {
				t.Fatalf("content missing signed link %q:\n%s", *img.URL, a.Content)
			}
		}
		if !strings.Contains(a.Content, "![ext](https://other.example.com/a.png)") {
			t.Fatalf("an unmirrored external link must stay untouched:\n%s", a.Content)
		}

		// Round-trip: the client echoes the signed content back on a save.
		if got := relinkKeys(a.Content, imageKeys(images()), h.storage.PublicURL); got != stored {
			t.Fatalf("unsign round-trip:\n got %q\nwant %q", got, stored)
		}
	})

	t.Run("a failed signature keeps the stored link", func(t *testing.T) {
		h := &AssetsHandler{storage: decorateStubStorage{failKey: k1}}
		a := &models.Asset{Content: stored}
		h.attachImages(t.Context(), a, images())
		if a.Images[1].URL != nil {
			t.Fatalf("images[1].url = %q, want unset", *a.Images[1].URL)
		}
		if !strings.Contains(a.Content, "("+pubBase+k1+" ") {
			t.Fatalf("unsigned image keeps its stored link:\n%s", a.Content)
		}
	})

	t.Run("public read leaves the Markdown as stored", func(t *testing.T) {
		h := &AssetsHandler{storage: decorateStubStorage{}, publicRead: true}
		a := &models.Asset{Content: stored}
		h.attachImages(t.Context(), a, images())
		if a.Content != stored {
			t.Fatalf("content changed under public read:\n%s", a.Content)
		}
		if *a.Images[0].URL != pubBase+k0 {
			t.Fatalf("images[0].url = %q, want public", *a.Images[0].URL)
		}
	})
}

// TestRelinkKeys_KeyBoundaries guards the matcher: a key that is a prefix of
// another's path segment, or that appears outside a link, must not be rewritten
// into the wrong URL.
func TestRelinkKeys_KeyBoundaries(t *testing.T) {
	const k1, k10 = "t/x/assets/u/images/1.png", "t/x/assets/u/images/10.png"
	in := "![a](" + pubBase + k1 + ") ![b](" + pubBase + k10 + ")"
	got := relinkKeys(in, []string{k1, k10}, func(k string) string { return "S:" + k })
	want := "![a](S:" + k1 + ") ![b](S:" + k10 + ")"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
