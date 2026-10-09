package flowkit

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// SkippedAsset is a campaign asset left out of a flow's sources: Status is
// its failed/partial status, or "" when the asset could not be loaded.
type SkippedAsset struct {
	ID     string
	Status string
}

// ReadyCampaignAssetIDs resolves the assets a campaign's flows may draw on:
// the campaign's explicit AssetIDs, in order, when it has any, otherwise every
// tenant asset. Assets in a definitive failure state (failed, partial) are
// left out; pending or processing ones stay, since they still contribute any
// chunks they already have. skipped lists the explicit IDs left out, so a
// caller can warn about them. One query either way.
func ReadyCampaignAssetIDs(ctx context.Context, assets repository.AssetRepository, campaign *models.Campaign) (ready []string, skipped []SkippedAsset, err error) {
	if len(campaign.AssetIDs) == 0 {
		all, err := assets.ListMeta(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		ready = make([]string, 0, len(all))
		for _, a := range all {
			if usableAssetStatus(a.Status) {
				ready = append(ready, a.ID)
			}
		}
		return ready, nil, nil
	}

	metas, err := assets.ListMeta(ctx, campaign.AssetIDs)
	if err != nil {
		return nil, nil, err
	}
	status := make(map[string]string, len(metas))
	for _, a := range metas {
		status[a.ID] = a.Status
	}
	ready = make([]string, 0, len(campaign.AssetIDs))
	for _, id := range campaign.AssetIDs {
		s, ok := status[id]
		switch {
		case !ok:
			skipped = append(skipped, SkippedAsset{ID: id})
		case !usableAssetStatus(s):
			skipped = append(skipped, SkippedAsset{ID: id, Status: s})
		default:
			ready = append(ready, id)
		}
	}
	return ready, skipped, nil
}

func usableAssetStatus(status string) bool {
	return status != models.AssetStatusFailed && status != models.AssetStatusPartial
}
