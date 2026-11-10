package assistantundo

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type assetTagsState struct {
	// Assets maps each asset's id to its tag ids.
	Assets map[string][]string `json:"assets"`
}

// AssetTags is the tag ids of assets, by asset id, for the undo log.
func AssetTags(tags map[string][]string) models.JSONMap {
	if tags == nil {
		tags = map[string][]string{}
	}
	return toMap(assetTagsState{Assets: tags})
}

// WithAssets lets s undo tags added to or removed from assets.
func (s *Service) WithAssets(assets repository.AssetRepository) *Service {
	s.undoers[models.AssistantActionAssetTags] = assetTags{assets}
	return s
}

type assetTags struct {
	assets repository.AssetRepository
}

// Undo gives every asset its tags back, all or none, while each still has
// the tags the change left it; tags the change created stay in the list.
func (u assetTags) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after assetTagsState
	if err := fromMap(a.Before, &before); err != nil {
		return err
	}
	if err := fromMap(a.After, &after); err != nil {
		return err
	}
	ok, err := u.assets.SetTagsIf(ctx, tx, after.Assets, before.Assets)
	if err != nil {
		return err
	}
	if !ok {
		return ErrChangedSince
	}
	return nil
}
