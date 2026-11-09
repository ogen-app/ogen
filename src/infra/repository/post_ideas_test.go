package repository_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// A campaign's posts map to their ideas in order; another campaign's posts
// and another workspace's links are left out.
func TestPostIdeas_IdeasOfCampaignPosts(t *testing.T) {
	db := openMigratedDB(t)
	links := repository.NewPostIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-campaign-links")

	for _, id := range []string{"links-c", "links-other"} {
		c := &models.Campaign{ID: id, Name: id, CampaignTypeID: "t", CreatedBy: "u1",
			AssetIDs: models.StringSlice{}, PublishingDays: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{}}
		if _, err := db.NewInsert().Model(c).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []struct{ id, campaign string }{{"links-a", "links-c"}, {"links-b", "links-c"}, {"links-x", "links-other"}} {
		post := &models.Post{ID: p.id, CampaignID: p.campaign, Status: models.PostStatusDraft, UsedAssetIDs: models.StringSlice{}}
		if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for post, ideas := range map[string][]string{"links-a": {"i2", "i1"}, "links-x": {"i1"}} {
		if err := links.Link(ctx, db, post, ideas); err != nil {
			t.Fatal(err)
		}
	}

	got, err := links.IdeasOfCampaignPosts(ctx, "links-c")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(got)), []string{"links-a"}) || !slices.Equal(got["links-a"], []string{"i2", "i1"}) {
		t.Fatalf("links = %v", got)
	}
	other, err := links.IdeasOfCampaignPosts(tenantctx.With(t.Context(), "tenant-elsewhere"), "links-c")
	if err != nil || len(other) != 0 {
		t.Fatalf("another workspace = %v, %v", other, err)
	}
}
