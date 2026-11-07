package clone_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
)

// A copy of a planned post stays planned and takes its theses with it; other
// notes stay on the source.
func TestCloneOfAPlannedPostCopiesItsTheses(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	// One connection with FKs off, so posts can be seeded without a campaign.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)
	posts := repository.NewPostRepository(db)
	notes := repository.NewPostNoteRepository(db)

	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	src := &models.Post{
		ID: id, CampaignID: "camp-1", Title: "t",
		MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: models.PostStatusPlanned, CTAType: models.CTATypeNone, CreatedBy: "user-1",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := posts.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, n := range []models.PostNote{
		{Type: models.PostNoteTypeDraftThesis, Body: "- the point", Origin: models.PostNoteOriginContentPlan},
		{Type: models.PostNoteTypeNote, Body: "aside", Origin: models.PostNoteOriginManual},
	} {
		if n.ID, err = models.NewID(); err != nil {
			t.Fatal(err)
		}
		n.PostID, n.CreatedBy = id, "user-1"
		if err := notes.Create(ctx, &n); err != nil {
			t.Fatalf("seed note: %v", err)
		}
	}

	svc := clone.New(db, posts, repository.NewPostVersionRepository(db), repository.NewPostAttachmentRepository(db),
		repository.NewPlatformRepository(db), repository.NewPostLogRepository(db), nil, nil)
	res, err := svc.Clone(ctx, id, clone.DefaultOptions("user-1", clone.TriggerAPI))
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if res.Post.Status != models.PostStatusPlanned {
		t.Fatalf("clone status = %s, want planned", res.Post.Status)
	}
	copied, err := notes.ListByPostID(ctx, res.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || copied[0].Type != models.PostNoteTypeDraftThesis || copied[0].Body != "- the point" ||
		copied[0].Origin != models.PostNoteOriginContentPlan || copied[0].ID == "" {
		t.Fatalf("clone notes = %+v, want the one thesis", copied)
	}
	if kept, err := notes.ListByPostID(ctx, id); err != nil || len(kept) != 2 {
		t.Fatalf("source notes = %d, err = %v, want both kept", len(kept), err)
	}
}
