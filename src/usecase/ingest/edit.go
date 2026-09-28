package ingest

import (
	"errors"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

var (
	// ErrContentLocked is a content change on a service-ingested asset, whose
	// content is ingestion output (re-extract instead).
	ErrContentLocked = errors.New("content of an ingested asset can't be edited — re-extract it instead")
	// ErrContentRequired is an empty content on an authored (MD/URL/plain) asset.
	ErrContentRequired = errors.New("content is required")
)

// Reembed is how an edited asset's embeddings are refreshed.
type Reembed int

const (
	// ReembedNone leaves the stored chunks as they are.
	ReembedNone Reembed = iota
	// ReembedImage re-embeds an edited image description through the image
	// pipeline, keeping its region chunks.
	ReembedImage
	// ReembedText re-chunks title + content (authored assets).
	ReembedText
)

func isImage(a *models.Asset) bool {
	return a.Type != nil && *a.Type == models.AssetTypeImage
}

// EditContent applies the per-type content rules to an edit, returning the
// content to store. The rules depend on the type, known only after the load:
//   - IMG: the description may be empty and is editable.
//   - PDF/DOC/AUDIO: content is read-only ingestion output; empty or unchanged
//     means "keep it" (so a title/tag-only save works), a different value is
//     ErrContentLocked.
//   - everything else: content is required.
func EditContent(a *models.Asset, content string) (string, error) {
	switch {
	case isImage(a):
		return content, nil
	case models.IsServiceIngestedAssetType(a.Type):
		if strings.TrimSpace(content) == "" {
			return a.Content, nil
		}
		if content != a.Content {
			return "", ErrContentLocked
		}
		return content, nil
	case strings.TrimSpace(content) == "":
		return "", ErrContentRequired
	}
	return content, nil
}

// ReembedAfterEdit decides how embeddings refresh when a (not yet updated)
// asset gets title and content. Service-ingested chunks carry source anchors
// and don't embed the title, so the text re-embed never runs for them — it
// would replace them with plain title+content chunks. Only a real change of
// the embedding inputs triggers a re-embed.
func ReembedAfterEdit(a *models.Asset, title, content string) Reembed {
	switch {
	case isImage(a):
		if a.Content != content {
			return ReembedImage
		}
	case models.IsServiceIngestedAssetType(a.Type):
	case a.Title != title || a.Content != content:
		return ReembedText
	}
	return ReembedNone
}
