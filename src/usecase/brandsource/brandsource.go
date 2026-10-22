// Package brandsource marks the assets brought in to read a brand from: files
// uploaded from the Brand offer and pages a website read imports. A marked
// asset carries a visible "Brand source" tag for people and the
// brand_distill origin for the system; only the origin changes behaviour, so
// renaming or deleting the tag changes nothing.
package brandsource

import (
	"context"
	"fmt"
	"slices"

	"github.com/ogen-app/ogen/src/domain/models"
)

const (
	// Purpose is the upload field value that asks for the mark.
	Purpose = "brand_source"
	// TagName is the workspace tag marked assets carry.
	TagName = "Brand source"
)

// ErrUnknownPurpose is an upload purpose other than Purpose.
var ErrUnknownPurpose = fmt.Errorf("purpose must be %q or absent", Purpose)

// ParsePurpose reports whether an upload asked to be marked; "" asks for
// nothing and anything but Purpose is ErrUnknownPurpose.
func ParsePurpose(purpose string) (bool, error) {
	switch purpose {
	case "":
		return false, nil
	case Purpose:
		return true, nil
	}
	return false, ErrUnknownPurpose
}

// Tags finds or creates the workspace tag. repository.TagRepository
// implements it.
type Tags interface {
	FindOrCreateByName(ctx context.Context, name, color, createdBy string) (*models.Tag, error)
}

// Marker stamps new assets as brand sources.
type Marker struct {
	Tags Tags
}

// Mark is what one marked asset receives.
type Mark struct {
	TagID string
	Role  string
	RunID string
}

// Prepare resolves the tag for a batch of assets created by userID. A nil
// Marker or tag store marks without a tag.
func (m *Marker) Prepare(ctx context.Context, role, runID, userID string) (Mark, error) {
	mark := Mark{Role: role, RunID: runID}
	if m == nil || m.Tags == nil {
		return mark, nil
	}
	tag, err := m.Tags.FindOrCreateByName(ctx, TagName, "", userID)
	if err != nil {
		return Mark{}, fmt.Errorf("find the %q tag: %w", TagName, err)
	}
	mark.TagID = tag.ID
	return mark, nil
}

// Apply stamps a not-yet-stored asset with the mark.
func (k Mark) Apply(a *models.Asset) {
	a.Origin = models.AssetOriginBrandDistill
	a.OriginRef = &models.AssetOriginRef{Role: k.Role, RunID: k.RunID}
	if k.TagID != "" && !slices.Contains(a.TagIDs, k.TagID) {
		a.TagIDs = append(a.TagIDs, k.TagID)
	}
}

// IsSource reports whether an asset was brought in to read a brand from.
func IsSource(a *models.Asset) bool {
	return a != nil && a.Origin == models.AssetOriginBrandDistill
}
