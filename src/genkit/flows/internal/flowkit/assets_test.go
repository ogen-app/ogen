package flowkit

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// metaAssets serves ListMeta from a fixed set; the rest is unused here.
type metaAssets struct {
	repository.AssetRepository
	all []models.Asset
}

func (m metaAssets) ListMeta(_ context.Context, ids []string) ([]models.Asset, error) {
	if ids == nil {
		return m.all, nil
	}
	var out []models.Asset
	for _, a := range m.all {
		if slices.Contains(ids, a.ID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func TestReadyCampaignAssetIDs(t *testing.T) {
	repo := metaAssets{all: []models.Asset{
		{ID: "ready", Status: models.AssetStatusReady},
		{ID: "pending", Status: models.AssetStatusPending},
		{ID: "failed", Status: models.AssetStatusFailed},
		{ID: "partial", Status: models.AssetStatusPartial},
	}}

	ready, skipped, err := ReadyCampaignAssetIDs(t.Context(), repo, &models.Campaign{})
	if err != nil || !slices.Equal(ready, []string{"ready", "pending"}) || skipped != nil {
		t.Fatalf("all assets: ready=%v skipped=%v err=%v", ready, skipped, err)
	}

	campaign := &models.Campaign{AssetIDs: models.StringSlice{"pending", "gone", "failed", "ready"}}
	ready, skipped, err = ReadyCampaignAssetIDs(t.Context(), repo, campaign)
	if err != nil || !slices.Equal(ready, []string{"pending", "ready"}) {
		t.Fatalf("explicit assets: ready=%v err=%v, want [pending ready] in campaign order", ready, err)
	}
	want := []SkippedAsset{{ID: "gone"}, {ID: "failed", Status: models.AssetStatusFailed}}
	if !slices.Equal(skipped, want) {
		t.Fatalf("skipped = %v, want %v", skipped, want)
	}
}
