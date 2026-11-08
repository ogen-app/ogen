package clone_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
)

// A clone comes from the same ideas as its source, in the same order.
func TestCloneKeepsTheSourceIdeas(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	// One connection with FKs off, so posts and links need no campaign or ideas.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)
	posts := repository.NewPostRepository(db)
	links := repository.NewPostIdeaRepository(db)

	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := posts.Create(ctx, &models.Post{
		ID: id, CampaignID: "camp-1", Title: "t", Content: "body",
		MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: models.PostStatusDraft, CTAType: models.CTATypeNone, CreatedBy: "user-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := links.Link(ctx, db, id, []string{"idea-b", "idea-a"}); err != nil {
		t.Fatal(err)
	}

	svc := clone.New(db, posts, repository.NewPostVersionRepository(db), repository.NewPostAttachmentRepository(db),
		repository.NewPlatformRepository(db), repository.NewPostLogRepository(db), nil, nil)
	res, err := svc.Clone(ctx, id, clone.DefaultOptions("user-1", clone.TriggerAPI))
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	var got []models.PostIdea
	if err := db.NewSelect().Model(&got).Where("pi.post_id = ?", res.Post.ID).OrderExpr("pi.position").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, l := range got {
		ids[i] = l.IdeaID
	}
	if !slices.Equal(ids, []string{"idea-b", "idea-a"}) || got[0].TenantID != models.DefaultTenantID {
		t.Fatalf("clone ideas = %+v, want idea-b, idea-a", got)
	}
}
