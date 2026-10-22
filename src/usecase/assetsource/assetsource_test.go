package assetsource_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/assetsource"
)

type chunks []models.AssetChunk

func (c chunks) GetByAssetID(context.Context, string) ([]models.AssetChunk, error) { return c, nil }

func TestReadBodyIntoParts(t *testing.T) {
	typ := models.AssetTypeURL
	body := strings.Repeat("a", 4000) + "\n\n" + strings.Repeat("b", 4000) + "\n\nshort"
	src, err := assetsource.Read(t.Context(), chunks(nil), &models.Asset{ID: "a1", Title: "About", Type: &typ, Content: body})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Segments) != 2 || src.Segments[0].Label != "Part 1" || !strings.HasSuffix(src.Segments[1].Text, "short") {
		t.Fatalf("segments = %d %+v", len(src.Segments), src.Segments[0].Label)
	}
}

func TestReadChunksWithLabels(t *testing.T) {
	typ := models.AssetTypeAudio
	slide, p3, p5 := "Slide 4", 3, 5
	rows := chunks{
		{Content: "one", SourceLabel: &slide, TokenCount: 7},
		{Content: "two", PageStart: &p3, PageEnd: &p5},
		{Content: "three", PageStart: &p3},
		{Content: "four"},
	}
	src, err := assetsource.Read(t.Context(), rows, &models.Asset{ID: "a2", Type: &typ})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Slide 4", "pp. 3–5", "p. 3", "Part 4"}
	for i, s := range src.Segments {
		if s.Label != want[i] {
			t.Errorf("segment %d label = %q, want %q", i, s.Label, want[i])
		}
	}
	if !src.Recording || src.Segments[0].Tokens != 7 {
		t.Fatalf("recording = %v, tokens = %d", src.Recording, src.Segments[0].Tokens)
	}
}
