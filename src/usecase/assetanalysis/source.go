package assetanalysis

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/usecase/assetsource"
)

// buildSource reads an asset's analysable text as the flow's source.
func (s *Service) buildSource(ctx context.Context, asset *models.Asset) (asset_analysis.Source, error) {
	read, err := assetsource.Read(ctx, s.repos.Chunks, asset)
	src := asset_analysis.Source{AssetID: asset.ID, Title: asset.Title, Kind: asset_analysis.SourceKindDocument}
	if read.Recording {
		src.Kind = asset_analysis.SourceKindRecording
	}
	for _, seg := range read.Segments {
		src.Segments = append(src.Segments, asset_analysis.Segment{Label: seg.Label, Text: seg.Text, Tokens: seg.Tokens})
	}
	return src, err
}
