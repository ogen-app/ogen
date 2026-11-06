package compose_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

// Compile writes the planned post, its theses, the links to its ideas and the
// audit entry together, and the link reads back both ways.
func TestCompile_WritesPostThesesAndLinks(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	// One connection with FKs off, so the campaign needs no type or owner.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)

	campaigns := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db))
	ideas := repository.NewIdeaRepository(db)
	links := repository.NewPostIdeaRepository(db)
	logs := repository.NewPostLogRepository(db)
	posts := repository.NewPostRepository(db)
	notes := repository.NewPostNoteRepository(db)

	if _, err := db.NewInsert().Model(&models.Campaign{
		ID: "cmp-compile", Name: "Spring", CampaignTypeID: "t", CreatedBy: "u1",
		AssetIDs: models.StringSlice{}, PublishingDays: models.StringSlice{},
		TargetPlatforms: models.CampaignPlatforms{{ID: "linkedin"}},
	}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	campaign := "cmp-compile"
	for _, i := range []*models.Idea{
		{ID: "idc-1", Title: "Pricing myths", Note: "Most buyers overpay", CampaignID: &campaign,
			Plan: models.IdeaPlan{PlatformID: new("linkedin")}},
		{ID: "idc-2", Title: "Founder story", CampaignID: &campaign},
	} {
		if err := ideas.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}

	svc := compose.New(db, ideas, campaigns, links, logs, nil)
	res, err := svc.Compile(ctx, compose.Input{IdeaIDs: []string{"idc-2", "idc-1"}, Actor: "u1"})
	if err != nil {
		t.Fatal(err)
	}

	post, err := posts.GetByID(ctx, res.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != models.PostStatusPlanned || post.Title != "Founder story" || post.PlatformID != "linkedin" || post.CampaignID != campaign {
		t.Fatalf("post = %+v", post)
	}
	ns, err := notes.ListByPostID(ctx, post.ID)
	if err != nil || len(ns) != 1 || ns[0].Type != models.PostNoteTypeDraftThesis {
		t.Fatalf("notes = %+v, %v", ns, err)
	}

	srcs, err := links.IdeasOfPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{srcs[0].ID, srcs[1].ID}; !slices.Equal(got, []string{"idc-2", "idc-1"}) {
		t.Fatalf("source ideas = %v; want the compile order", got)
	}
	refs, err := links.PostsOfIdea(ctx, "idc-1")
	if err != nil || len(refs) != 1 || refs[0].ID != post.ID || refs[0].Status != models.PostStatusPlanned {
		t.Fatalf("posts of idea = %+v, %v", refs, err)
	}

	// One idea feeds a second post; linking again keeps one row per pair.
	if _, err := svc.Compile(ctx, compose.Input{IdeaIDs: []string{"idc-1"}, Actor: "u1", Title: "LinkedIn take"}); err != nil {
		t.Fatal(err)
	}
	if err := links.Link(ctx, db, post.ID, []string{"idc-1"}); err != nil {
		t.Fatal(err)
	}
	if refs, _ = links.PostsOfIdea(ctx, "idc-1"); len(refs) != 2 {
		t.Fatalf("idea feeds %d posts, want 2", len(refs))
	}

	// Another workspace sees no links.
	other := tenantctx.With(context.Background(), "tenant-other")
	if got, err := links.IdeasOfPost(other, post.ID); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read %v (%v)", got, err)
	}
}
