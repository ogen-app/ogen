package assetanalysis

import (
	"context"
	"fmt"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
)

// sectionRunes sizes the sections a markdown or web-page body is cut into so
// the model has labels to cite; about 1500 tokens each.
const sectionRunes = 6000

// buildSource reads an asset's analysable text. Service-ingested assets
// (PDF, DOC, AUDIO) are read from their chunks, which carry citation labels;
// markdown and web pages from their body, because their chunks overlap.
func (s *Service) buildSource(ctx context.Context, asset *models.Asset) (asset_analysis.Source, error) {
	src := asset_analysis.Source{AssetID: asset.ID, Title: asset.Title, Kind: asset_analysis.SourceKindDocument}
	if asset.Type != nil && *asset.Type == models.AssetTypeAudio {
		src.Kind = asset_analysis.SourceKindRecording
	}
	if !models.IsServiceIngestedAssetType(asset.Type) && strings.TrimSpace(asset.Content) != "" {
		src.Segments = sections(asset.Content)
		return src, nil
	}
	chunks, err := s.repos.Chunks.GetByAssetID(ctx, asset.ID)
	if err != nil {
		return src, fmt.Errorf("load chunks: %w", err)
	}
	for i, c := range chunks {
		src.Segments = append(src.Segments, asset_analysis.Segment{
			Label:  chunkLabel(c, i),
			Text:   c.Content,
			Tokens: c.TokenCount,
		})
	}
	return src, nil
}

// chunkLabel is a chunk's citation: its own label, else its page range, else
// its position.
func chunkLabel(c models.AssetChunk, i int) string {
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

// sections cuts a body into paragraph-aligned parts labelled "Part N".
func sections(body string) []asset_analysis.Segment {
	var out []asset_analysis.Segment
	var cur strings.Builder
	flush := func() {
		if text := strings.TrimSpace(cur.String()); text != "" {
			out = append(out, asset_analysis.Segment{Label: fmt.Sprintf("Part %d", len(out)+1), Text: text})
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
