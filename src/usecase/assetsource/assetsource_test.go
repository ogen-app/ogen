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

// Each chunk's segment says where the chunk is: its page or page range, its
// slide, or its time range; a body cut into parts has none.
func TestReadLocatesChunks(t *testing.T) {
	typ := models.AssetTypePDF
	p3, p5, p6 := 3, 5, 6
	rows := chunks{
		{ID: "a3:0", Content: "one", PageStart: &p3, PageEnd: &p3},
		{ID: "a3:1", Content: "two", PageStart: &p5, PageEnd: &p6},
		{ID: "a3:2", Content: "three", SourceLabel: new("Slide 4"), SourceAnchor: &models.SourceAnchor{Kind: "slide", Slide: 4}},
		{ID: "a3:3", Content: "four", SourceLabel: new("0:00–0:42"), SourceAnchor: &models.SourceAnchor{Kind: "time", StartMs: 0, EndMs: 42000}},
		{ID: "a3:4", Content: "five", SourceAnchor: &models.SourceAnchor{Kind: "page", Page: 9}},
	}
	src, err := assetsource.Read(t.Context(), rows, &models.Asset{ID: "a3", Type: &typ})
	if err != nil {
		t.Fatal(err)
	}
	locs := make([]models.SourceLocator, len(src.Segments))
	for i, s := range src.Segments {
		if s.Locator == nil {
			t.Fatalf("segment %d has no locator", i)
		}
		locs[i] = *s.Locator
	}
	if l := locs[0]; l.ChunkID != "a3:0" || l.Label != "p. 3" || *l.Page != 3 || l.PageEnd != nil {
		t.Errorf("one page = %+v", l)
	}
	if l := locs[1]; l.Label != "pp. 5–6" || *l.Page != 5 || *l.PageEnd != 6 {
		t.Errorf("page range = %+v", l)
	}
	if l := locs[2]; l.Label != "Slide 4" || l.Page != nil || *l.Slide != 4 {
		t.Errorf("slide = %+v", l)
	}
	if l := locs[3]; l.StartMS == nil || *l.StartMS != 0 || *l.EndMS != 42000 || l.Page != nil {
		t.Errorf("time range = %+v", l)
	}
	if l := locs[4]; l.Page == nil || *l.Page != 9 {
		t.Errorf("anchor page = %+v", l)
	}

	body := models.AssetTypeURL
	parts, err := assetsource.Read(t.Context(), chunks(nil), &models.Asset{ID: "a4", Type: &body, Content: "text"})
	if err != nil || parts.Segments[0].Locator != nil {
		t.Fatalf("a part has a locator: %+v, %v", parts.Segments, err)
	}
}
