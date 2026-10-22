// Package assetsource reads an asset's settled text as citable segments, the
// way every flow that reads assets wants it: service-ingested assets (PDF,
// DOC, AUDIO, IMG) from their chunks, which carry citation labels, and
// markdown and web pages from their body, cut into labelled parts because
// their chunks overlap.
package assetsource

import (
	"context"
	"fmt"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// sectionRunes sizes the parts a markdown or web-page body is cut into; about
// 1500 tokens each.
const sectionRunes = 6000

// Segment is one citable piece of an asset, in reading order.
type Segment struct {
	// Label is how the segment is cited: "p. 12", "Slide 4", "12:03–12:47",
	// or "Part N" for a body without anchors.
	Label string
	Text  string
	// Tokens is the ingest-reported token count; zero when unknown.
	Tokens int
}

// Source is an asset's text.
type Source struct {
	AssetID string
	Title   string
	// Recording is set for a transcript.
	Recording bool
	Segments  []Segment
}

// Chunks reads an asset's ingested chunks in order.
// repository.AssetChunkRepository implements it.
type Chunks interface {
	GetByAssetID(ctx context.Context, assetID string) ([]models.AssetChunk, error)
}

// Read returns the asset's text as segments.
func Read(ctx context.Context, chunks Chunks, asset *models.Asset) (Source, error) {
	src := Source{AssetID: asset.ID, Title: asset.Title}
	if asset.Type != nil && *asset.Type == models.AssetTypeAudio {
		src.Recording = true
	}
	if !models.IsServiceIngestedAssetType(asset.Type) && strings.TrimSpace(asset.Content) != "" {
		src.Segments = Sections(asset.Content)
		return src, nil
	}
	rows, err := chunks.GetByAssetID(ctx, asset.ID)
	if err != nil {
		return src, fmt.Errorf("load chunks: %w", err)
	}
	for i, c := range rows {
		src.Segments = append(src.Segments, Segment{Label: ChunkLabel(c, i), Text: c.Content, Tokens: c.TokenCount})
	}
	return src, nil
}

// ChunkLabel is a chunk's citation: its own label, else its page range, else
// its position.
func ChunkLabel(c models.AssetChunk, i int) string {
	if c.SourceLabel != nil && *c.SourceLabel != "" {
		return *c.SourceLabel
	}
	switch {
	case c.PageStart != nil && c.PageEnd != nil && *c.PageEnd > *c.PageStart:
		return fmt.Sprintf("pp. %d–%d", *c.PageStart, *c.PageEnd)
	case c.PageStart != nil:
		return fmt.Sprintf("p. %d", *c.PageStart)
	}
	return fmt.Sprintf("Part %d", i+1)
}

// Sections cuts a body into paragraph-aligned parts labelled "Part N".
func Sections(body string) []Segment {
	var out []Segment
	var cur strings.Builder
	flush := func() {
		if text := strings.TrimSpace(cur.String()); text != "" {
			out = append(out, Segment{Label: fmt.Sprintf("Part %d", len(out)+1), Text: text})
		}
		cur.Reset()
	}
	for para := range strings.SplitSeq(body, "\n\n") {
		if cur.Len() > 0 && cur.Len()+len(para) > sectionRunes {
			flush()
		}
		cur.WriteString(para)
		cur.WriteString("\n\n")
	}
	flush()
	return out
}
