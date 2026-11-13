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

// A post read with its relations says how many ideas it was made from, and
// an idea's posts carry their post type; another workspace's links count
// for nothing.
func TestPostIdeas_CountAndPostType(t *testing.T) {
	db := openMigratedDB(t)
	links := repository.NewPostIdeaRepository(db)
	posts := repository.NewPostRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-idea-counts")
	other := tenantctx.With(t.Context(), "tenant-idea-counts-other")

	c := &models.Campaign{ID: "count-c", Name: "count-c", CampaignTypeID: "t", CreatedBy: "u1",
		AssetIDs: models.StringSlice{}, PublishingDays: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{}}
	if _, err := db.NewInsert().Model(c).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, postType string }{{"count-two", "carousel"}, {"count-none", ""}} {
		post := &models.Post{ID: p.id, CampaignID: "count-c", PlatformPostType: p.postType, Status: models.PostStatusDraft,
			MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{}, CTAType: models.CTATypeNone, CreatedBy: "u1"}
		if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := links.Link(ctx, db, "count-two", []string{"i1", "i2"}); err != nil {
		t.Fatal(err)
	}
	// A link in another workspace to the same post id isn't the caller's.
	if err := links.Link(other, db, "count-two", []string{"i3"}); err != nil {
		t.Fatal(err)
	}

	list, err := posts.ListByCampaign(ctx, "count-c")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, p := range list {
		counts[p.ID] = p.IdeaCount
	}
	if counts["count-two"] != 2 || counts["count-none"] != 0 || len(counts) != 2 {
		t.Fatalf("idea counts = %v", counts)
	}
	refs, err := links.PostsOfIdea(ctx, "i1")
	if err != nil || len(refs) != 1 || refs[0].PlatformPostType != "carousel" {
		t.Fatalf("posts of idea = %+v, %v", refs, err)
	}
}
