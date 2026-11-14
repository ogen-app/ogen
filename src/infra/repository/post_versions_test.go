package repository_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// TestPostVersionGetLatestByPostID pins the two behaviours that CON-184's
// resolveAssessedContent relies on from the *real* Bun-backed repository, which
// the fake in resolve_content_test.go can only assume:
//
//   - a post with no versions yields (nil, nil) — the sql.ErrNoRows→nil mapping
//     that the quality flow's fallback to posts.content depends on. If this
//     regressed to a raw error, versionless posts would stop being assessable.
//   - a post with several versions yields the highest version_number.
func TestPostVersionGetLatestByPostID(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostVersionRepository(db)

	// Missing record → (nil, nil), not an error.
	got, err := repo.GetLatestByPostID(ctx, "no-such-post")
	if err != nil {
		t.Fatalf("missing post must not error: %v", err)
	}
	if got != nil {
		t.Fatalf("missing post must return a nil version, got %+v", got)
	}

	// Seed a post and two versions out of numeric order to prove the result is
	// chosen by version_number (DESC), not insertion/created order.
	seedPost(t, db, "post-v", "", "", time.Now().UTC())
	seedVersion(t, db, "ver-2", "post-v", 2, "second draft")
	seedVersion(t, db, "ver-1", "post-v", 1, "first draft")

	got, err = repo.GetLatestByPostID(ctx, "post-v")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got == nil {
		t.Fatal("expected the latest version, got nil")
	}
	if got.VersionNumber != 2 || got.Content != "second draft" {
		t.Fatalf("got v%d %q, want v2 %q", got.VersionNumber, got.Content, "second draft")
	}
}

// seedVersion inserts a post_versions row via the tenant-scoped context so the
// CON-97 hook populates tenant_id, mirroring seedPost.
func seedVersion(t *testing.T, db *bun.DB, id, postID string, num int, content string) {
	t.Helper()
	v := &models.PostVersion{
		ID:            id,
		PostID:        postID,
		VersionNumber: num,
		Content:       content,
		Note:          "",
		Creator:       "user",
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := db.NewInsert().Model(v).Exec(tenantCtx()); err != nil {
		t.Fatalf("seed version %s: %v", id, err)
	}
}

// TestPostVersionCreateNextConcurrent proves concurrent writers each get a
// distinct, gap-free version_number instead of colliding on the unique index.
func TestPostVersionCreateNextConcurrent(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostVersionRepository(db)
	seedPost(t, db, "post-c", "", "", time.Now().UTC())
	seedVersion(t, db, "ver-c1", "post-c", 1, "seed")

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Go(func() {
			errs <- repo.CreateNext(ctx, &models.PostVersion{
				ID:      fmt.Sprintf("ver-c-%d", i),
				PostID:  "post-c",
				Content: "concurrent",
				Creator: "user",
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("create next: %v", err)
		}
	}

	versions, err := repo.ListByPostID(ctx, "post-c")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != writers+1 {
		t.Fatalf("got %d versions, want %d", len(versions), writers+1)
	}
	for i, v := range versions {
		if v.VersionNumber != i+1 {
			t.Fatalf("version[%d] = %d, want %d", i, v.VersionNumber, i+1)
		}
	}
}

// A version saved without a CreatedBy is credited to the user the context
// acts for; one that names its person keeps it.
func TestPostVersionCreatedBy(t *testing.T) {
	db := openMigratedDB(t)
	ctx := logging.WithUserID(tenantCtx(), "u-editor")
	repo := repository.NewPostVersionRepository(db)
	seedPost(t, db, "post-by", "", "", time.Now().UTC())

	if err := repo.CreateNext(ctx, &models.PostVersion{ID: "by-1", PostID: "post-by", Content: "a", Creator: models.PostVersionCreatorUser}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateNext(ctx, &models.PostVersion{ID: "by-2", PostID: "post-by", Content: "b", Creator: models.PostVersionCreatorAssistant, CreatedBy: "u-asker"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateNext(tenantCtx(), &models.PostVersion{ID: "by-3", PostID: "post-by", Content: "c", Creator: models.PostVersionCreatorSystem}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ListByPostID(tenantCtx(), "post-by")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].CreatedBy != "u-editor" || got[1].CreatedBy != "u-asker" || got[2].CreatedBy != "" {
		t.Fatalf("created_by = %q %q %q", got[0].CreatedBy, got[1].CreatedBy, got[2].CreatedBy)
	}
}

// A post is edited once a person saved copy for it; the post assistant's
// snapshot of the post as it was, and the assistant's and the system's
// versions, aren't edits.
func TestPostVersionEditedPostIDs(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostVersionRepository(db)
	for _, id := range []string{"post-ed", "post-snap", "post-none"} {
		seedPost(t, db, id, "", "", time.Now().UTC())
	}
	for _, v := range []models.PostVersion{
		{ID: "ed-1", PostID: "post-ed", Content: "a", Creator: models.PostVersionCreatorUser, Note: models.PostVersionNoteInitial},
		{ID: "ed-2", PostID: "post-ed", Content: "b", Creator: models.PostVersionCreatorUser},
		{ID: "snap-1", PostID: "post-snap", Content: "a", Creator: models.PostVersionCreatorUser, Note: models.PostVersionNoteInitial},
		{ID: "snap-2", PostID: "post-snap", Content: "b", Creator: models.PostVersionCreatorAssistant},
		{ID: "snap-3", PostID: "post-snap", Content: "b", Creator: models.PostVersionCreatorSystem, Note: models.PostVersionNoteMarkedReady},
	} {
		if err := repo.CreateNext(ctx, &v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.EditedPostIDs(ctx, []string{"post-ed", "post-snap", "post-none"})
	if err != nil || len(got) != 1 || got[0] != "post-ed" {
		t.Fatalf("edited = %v, %v", got, err)
	}
	if got, err := repo.EditedPostIDs(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("none asked = %v, %v", got, err)
	}
}
