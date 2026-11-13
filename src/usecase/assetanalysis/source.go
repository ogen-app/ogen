package assetanalysis

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/usecase/assetsource"
)

// passages maps a segment label to where its first segment is. Labels repeat
// (several chunks of one page are all "p. 3"); the first in reading order
// stands for them all.
type passages map[string]*models.SourceLocator

// locate is where the first of labels is; nil when it has no chunk.
func (p passages) locate(labels []string) *models.SourceLocator {
	if len(labels) == 0 {
		return nil
	}
	return p[labels[0]]
}

// buildSource reads an asset's analysable text as the flow's source, with
// where each cited label is.
func (s *Service) buildSource(ctx context.Context, asset *models.Asset) (asset_analysis.Source, passages, error) {
	read, err := assetsource.Read(ctx, s.repos.Chunks, asset)
	src := asset_analysis.Source{AssetID: asset.ID, Title: asset.Title, Kind: asset_analysis.SourceKindDocument}
	if read.Recording {
		src.Kind = asset_analysis.SourceKindRecording
	}
	at := passages{}
	for _, seg := range read.Segments {
		src.Segments = append(src.Segments, asset_analysis.Segment{Label: seg.Label, Text: seg.Text, Tokens: seg.Tokens})
		if _, seen := at[seg.Label]; !seen && seg.Locator != nil {
			at[seg.Label] = seg.Locator
		}
	}
	return src, at, err
}
