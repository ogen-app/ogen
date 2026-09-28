package ingest

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func asset(typ, title, content string) *models.Asset {
	a := &models.Asset{Title: title, Content: content}
	if typ != "" {
		a.Type = &typ
	}
	return a
}

func TestEditContent(t *testing.T) {
	tests := []struct {
		name    string
		asset   *models.Asset
		content string
		want    string
		wantErr error
	}{
		{"image may be emptied", asset(models.AssetTypeImage, "t", "desc"), "", "", nil},
		{"ingested empty keeps", asset(models.AssetTypePDF, "t", "[]"), "  ", "[]", nil},
		{"ingested unchanged", asset(models.AssetTypeDocument, "t", "x"), "x", "x", nil},
		{"ingested changed is locked", asset(models.AssetTypeAudio, "t", "x"), "y", "", ErrContentLocked},
		{"authored requires content", asset(models.AssetTypeMarkdown, "t", "x"), " ", "", ErrContentRequired},
		{"untyped requires content", asset("", "t", "x"), "", "", ErrContentRequired},
		{"authored edit", asset(models.AssetTypeURL, "t", "x"), "y", "y", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EditContent(tt.asset, tt.content)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("got %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestReembedAfterEdit(t *testing.T) {
	tests := []struct {
		name           string
		asset          *models.Asset
		title, content string
		want           Reembed
	}{
		{"image description edit", asset(models.AssetTypeImage, "t", "a"), "t", "b", ReembedImage},
		{"image rename only", asset(models.AssetTypeImage, "t", "a"), "u", "a", ReembedNone},
		{"ingested rename never re-chunks", asset(models.AssetTypePDF, "t", "a"), "u", "a", ReembedNone},
		{"authored rename", asset(models.AssetTypeMarkdown, "t", "a"), "u", "a", ReembedText},
		{"authored no-op", asset(models.AssetTypeMarkdown, "t", "a"), "t", "a", ReembedNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReembedAfterEdit(tt.asset, tt.title, tt.content); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChecksumAndNilBlob(t *testing.T) {
	// SHA-256 of "abc".
	if got := Checksum([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("checksum = %s", got)
	}
	var b *Blob
	b.Discard(t.Context()) // nil-safe
	if (&Service{}).FindByChecksum(t.Context(), "x") != nil {
		t.Fatal("no file repo means no dedupe")
	}
}
