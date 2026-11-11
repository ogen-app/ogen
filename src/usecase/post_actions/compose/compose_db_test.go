package compose_test

import (
	"context"
	"errors"
	"slices"
	"strings"
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
		TargetPlatforms: models.CampaignPlatforms{{ID: "linkedin"}, {ID: "threads"}},
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

	post, err := posts.GetByID(ctx, res.Posts[0].Post.ID)
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

	entries, err := logs.ListByPostID(ctx, post.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].EventType != models.PostLogEventPostCompiled || entries[0].Actor != "u1" ||
		!strings.Contains(entries[0].Payload, `"idea_ids":["idc-2","idc-1"]`) {
		t.Fatalf("audit log = %+v; want one post_compiled by u1 over idc-2, idc-1", entries)
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

	// One idea feeds more posts, one per platform, each with its theses;
	// linking again keeps one row per pair.
	more, err := svc.Compile(ctx, compose.Input{IdeaIDs: []string{"idc-1"}, Actor: "u1", Title: "Two takes", PlatformIDs: []string{"linkedin", "threads"}})
	if err != nil || len(more.Posts) != 2 || more.Posts[0].Post.PlatformID != "linkedin" || more.Posts[1].Post.PlatformID != "threads" {
		t.Fatalf("compile for two platforms = %+v, %v", more, err)
	}
	for _, c := range more.Posts {
		if ns, err := notes.ListByPostID(ctx, c.Post.ID); err != nil || len(ns) != 1 || ns[0].ID != c.Note.ID {
			t.Fatalf("notes of %s = %+v, %v", c.Post.PlatformID, ns, err)
		}
	}
	if err := links.Link(ctx, db, post.ID, []string{"idc-1"}); err != nil {
		t.Fatal(err)
	}
	if refs, _ = links.PostsOfIdea(ctx, "idc-1"); len(refs) != 3 {
		t.Fatalf("idea feeds %d posts, want 3", len(refs))
	}

	// Another workspace sees no links.
	other := tenantctx.With(context.Background(), "tenant-other")
	if got, err := links.IdeasOfPost(other, post.ID); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read %v (%v)", got, err)
	}
}

// With the database's constraints on, a phase of the campaign's type compiles
// and a phase of another type is ErrInvalidPhase, with nothing written.
func TestCompile_PhaseMustBeTheCampaignTypes(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)

	// The seeded system types: phase 98 is the awareness type's (Uk), M2
	// another type's.
	const awareness, ownPhase, otherPhase = "Uk", "98", "M2"
	for _, q := range []string{
		"INSERT INTO accounts (id, email, name) VALUES ('acc-phase', 'phase@example.com', 'Phase')",
		"INSERT INTO users (id, name, email, tenant_id, account_id) VALUES ('u-phase', 'Phase', 'phase@example.com', 'default', 'acc-phase')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	campaigns := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db))
	ideas := repository.NewIdeaRepository(db)
	if _, err := db.NewInsert().Model(&models.Campaign{
		ID: "cmp-phase", Name: "Spring", CampaignTypeID: awareness, CreatedBy: "u-phase",
		AssetIDs: models.StringSlice{}, PublishingDays: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{},
	}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	campaign := "cmp-phase"
	if err := ideas.Create(ctx, &models.Idea{ID: "idp-1", Title: "Pricing myths", CampaignID: &campaign}); err != nil {
		t.Fatal(err)
	}
	logs := repository.NewPostLogRepository(db)
	svc := compose.New(db, ideas, campaigns, repository.NewPostIdeaRepository(db), logs, nil)

	if _, err := svc.Compile(ctx, compose.Input{IdeaIDs: []string{"idp-1"}, PhaseID: otherPhase, Actor: "u-phase"}); !errors.Is(err, compose.ErrInvalidPhase) {
		t.Fatalf("other type's phase: err = %v, want ErrInvalidPhase", err)
	}
	var count int
	if err := db.NewRaw("SELECT count(*) FROM posts").Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("posts after a refused compile = %d (%v), want 0", count, err)
	}

	res, err := svc.Compile(ctx, compose.Input{IdeaIDs: []string{"idp-1"}, PhaseID: ownPhase, Actor: "u-phase"})
	if err != nil {
		t.Fatalf("own type's phase: %v", err)
	}
	if res.Posts[0].Post.CampaignTypePhaseID == nil || *res.Posts[0].Post.CampaignTypePhaseID != ownPhase {
		t.Fatalf("phase = %v, want %s", res.Posts[0].Post.CampaignTypePhaseID, ownPhase)
	}
	if entries, err := logs.ListByPostID(ctx, res.Posts[0].Post.ID, 10); err != nil || len(entries) != 1 {
		t.Fatalf("audit log = %+v (%v), want the compile", entries, err)
	}
}
