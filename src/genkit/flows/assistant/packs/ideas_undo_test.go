package packs

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

// ideasDB is the ideas pack over a real database, with FKs off so the
// campaign needs no type or owner, and the undo service over it.
func ideasDB(t *testing.T) (context.Context, *bun.DB, Deps, *assistantundo.Service) {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), models.DefaultTenantID)
	ctx, _ = assistant.NewTurnContext(ctx, campaign1, nil)

	campaigns := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db))
	ideaRepo := repository.NewIdeaRepository(db)
	posts := repository.NewPostRepository(db)
	actions := repository.NewAssistantActionRepository(db)
	if _, err := db.NewInsert().Model(&models.Campaign{
		ID: "c1", Name: "Spring", CampaignTypeID: "t", CreatedBy: "u1",
		AssetIDs: models.StringSlice{}, PublishingDays: models.StringSlice{},
		TargetPlatforms: models.CampaignPlatforms{{ID: "linkedin"}, {ID: "threads"}},
	}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	c1 := "c1"
	for _, i := range []*models.Idea{
		{ID: "u-1", Title: "Pricing myths", CampaignID: &c1, Plan: models.IdeaPlan{PlatformID: new("linkedin")}},
		{ID: "u-2", Title: "Founder story", CampaignID: &c1},
		{ID: "u-3", Title: "Spa menu", CampaignID: &c1, SourceAssetID: new("a9")},
	} {
		if err := ideaRepo.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	d := Deps{Ideas: ideas.New(ideaRepo, nil), IdeaRepo: ideaRepo, Actions: actions, ProposalThreshold: 3,
		Compose: compose.New(db, ideaRepo, campaigns, repository.NewPostIdeaRepository(db), repository.NewPostLogRepository(db), nil)}
	return ctx, db, d, assistantundo.New(db, actions, campaigns).WithIdeas(ideaRepo).WithPosts(posts)
}

// Undoing a compile deletes its posts and their links while no one has
// changed them; their ideas stay. Once one post is edited, they all stay.
func TestCompileIdeas_Undo(t *testing.T) {
	ctx, db, d, undo := ideasDB(t)
	links := repository.NewPostIdeaRepository(db)

	out, err := compileIdeas(ctx, d, CompileIdeasInput{IdeaIDs: []string{"u-1", "u-2"}})
	if err != nil || out.UndoID == "" || out.Grounding.Mode != assistantkit.Exploratory {
		t.Fatalf("compile = %+v, %v", out, err)
	}
	if _, err := undo.Undo(ctx, out.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.NewPostRepository(db).GetByID(ctx, out.Posts[0].PostID); err == nil {
		t.Fatal("the compiled post outlived its undo")
	}
	if posts, _ := links.PostsOfIdea(ctx, "u-1"); len(posts) != 0 {
		t.Fatalf("links = %+v", posts)
	}
	if _, err := d.Ideas.Get(ctx, "u-1"); err != nil {
		t.Fatalf("the idea went with the post: %v", err)
	}

	out, err = compileIdeas(ctx, d, CompileIdeasInput{IdeaIDs: []string{"u-3", "u-1"}, PlatformIDs: []string{"linkedin", "threads"}})
	if err != nil || len(out.Posts) != 2 || out.Posts[1].PlatformID != "threads" || out.Posts[0].Date != "" || out.Posts[0].DateFrom != "" ||
		out.Grounding.Mode != assistantkit.Grounded || len(out.Grounding.Sources) != 1 || out.Grounding.Sources[0].ID != "a9" || out.Grounding.Sources[0].IdeaID != "u-3" {
		t.Fatalf("grounded compile for two platforms = %+v, %v", out, err)
	}
	if _, err := db.NewUpdate().Model((*models.Post)(nil)).Set("content = ?", "Written by hand").Where("id = ?", out.Posts[1].PostID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := undo.Undo(ctx, out.UndoID, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("undo with an edited post = %v; want changed since", err)
	}
	if _, err := repository.NewPostRepository(db).GetByID(ctx, out.Posts[0].PostID); err != nil {
		t.Fatalf("a refused undo deleted the unedited post: %v", err)
	}

	// A day named dates each post, and says so.
	day := "2026-12-01"
	dated, err := compileIdeas(ctx, d, CompileIdeasInput{IdeaIDs: []string{"u-1"}, PlatformIDs: []string{"linkedin"}, TargetDate: day})
	if err != nil || dated.Posts[0].Date != day || dated.Posts[0].DateFrom != "asked" {
		t.Fatalf("dated compile = %+v, %v", dated, err)
	}

	// Both posts as made: one undo deletes both.
	out, err = compileIdeas(ctx, d, CompileIdeasInput{IdeaIDs: []string{"u-2"}, PlatformIDs: []string{"linkedin", "threads"}})
	if err != nil || len(out.Posts) != 2 {
		t.Fatalf("compile = %+v, %v", out, err)
	}
	if _, err := undo.Undo(ctx, out.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Posts {
		if _, err := repository.NewPostRepository(db).GetByID(ctx, p.PostID); err == nil {
			t.Fatalf("post %s outlived the undo", p.PlatformID)
		}
	}

	// An entry of one post, as recorded before compiles made several, still
	// undoes.
	res, err := d.Compose.Compile(ctx, compose.Input{IdeaIDs: []string{"u-2"}, Actor: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Posts[0].Post
	id := recordUndo(ctx, d, models.AssistantActionPostCreated, p.ID, models.JSONMap{}, assistantundo.PostCreated(p, []models.PostNote{*res.Posts[0].Note}))
	if _, err := undo.Undo(ctx, id, "u1"); err != nil {
		t.Fatalf("undo of a one-post entry: %v", err)
	}
	if _, err := repository.NewPostRepository(db).GetByID(ctx, p.ID); err == nil {
		t.Fatal("the post outlived its one-post undo")
	}
}

// Undoing a bulk triage restores every idea's triage, or none when one was
// triaged again since.
func TestSetIdeaVerdicts_Undo(t *testing.T) {
	ctx, _, d, undo := ideasDB(t)
	out, err := setIdeaVerdicts(ctx, d, SetIdeaVerdictsInput{IdeaIDs: []string{"u-1", "u-2", "gone"}, Verdict: "yes"})
	if err != nil || out.Done != 2 || out.UndoID == "" {
		t.Fatalf("verdicts = %+v, %v", out, err)
	}
	if _, err := undo.Undo(ctx, out.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u-1", "u-2"} {
		if i, _ := d.Ideas.Get(ctx, id); i.Verdict != nil || i.DecidedAt != nil {
			t.Fatalf("%s = %+v; want back in the inbox", id, i)
		}
	}

	out, _ = setIdeaVerdicts(ctx, d, SetIdeaVerdictsInput{IdeaIDs: []string{"u-1", "u-2"}, Verdict: "no"})
	if _, err := setIdeaVerdict(ctx, d, SetIdeaVerdictInput{IdeaID: "u-2", Verdict: "yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := undo.Undo(ctx, out.UndoID, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("undo after a re-triage = %v; want changed since", err)
	}
	if i, _ := d.Ideas.Get(ctx, "u-1"); i.Verdict == nil || *i.Verdict != models.IdeaVerdictNo {
		t.Fatalf("u-1 = %+v; a refused undo must restore none", i)
	}
}
