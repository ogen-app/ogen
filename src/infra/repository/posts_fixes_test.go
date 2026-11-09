package repository_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func fixPost(t *testing.T, repo repository.PostRepository, at *time.Time, status models.PostStatus, postType string) *models.Post {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	p := &models.Post{ID: id, CampaignID: "camp-1", Title: "Post", MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: status, PlatformPostType: postType, CTAType: models.CTATypeNone, CreatedBy: "user-1", ScheduledAt: at,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.Create(tenantCtx(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	return p
}

// A date is cleared and set back only while the post still has the date the
// change saw and nothing is queued in Zernio for it.
func TestPostSetScheduleIf(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	at := time.Date(2026, 1, 5, 17, 0, 0, 0, time.UTC)
	other := at.Add(time.Hour)
	draft := fixPost(t, repo, &at, models.PostStatusDraft, "")
	moved := fixPost(t, repo, &other, models.PostStatusDraft, "")
	scheduled := fixPost(t, repo, &at, models.PostStatusScheduled, "")

	set, err := repo.SetScheduleIf(ctx, db, []repository.ScheduleChange{
		{PostID: draft.ID, From: &at}, {PostID: moved.ID, From: &at}, {PostID: scheduled.ID, From: &at},
	}, time.Now().UTC())
	if err != nil || !slices.Equal(set, []string{draft.ID}) {
		t.Fatalf("cleared = %v, %v", set, err)
	}
	if got, _ := repo.GetByID(ctx, draft.ID); got.ScheduledAt != nil {
		t.Fatalf("draft still dated %v", got.ScheduledAt)
	}
	if set, err = repo.SetScheduleIf(ctx, db, []repository.ScheduleChange{{PostID: draft.ID, To: &at}}, time.Now().UTC()); err != nil || len(set) != 1 {
		t.Fatalf("restored = %v, %v", set, err)
	}
	if got, _ := repo.GetByID(ctx, draft.ID); got.ScheduledAt == nil || !got.ScheduledAt.Equal(at) {
		t.Fatalf("draft dated %v, want %v", got.ScheduledAt, at)
	}
}

// A type changes only while the post still has the type and status the
// change saw, and isn't submitted.
func TestPostSetPostTypeIf(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	ready := fixPost(t, repo, nil, models.PostStatusReadyForPublish, "video")
	changed := fixPost(t, repo, nil, models.PostStatusDraft, "image")
	scheduled := fixPost(t, repo, nil, models.PostStatusScheduled, "video")

	set, err := repo.SetPostTypeIf(ctx, db, []repository.PostTypeChange{
		{PostID: ready.ID, From: "video", To: "text", FromStatus: models.PostStatusReadyForPublish, ToStatus: models.PostStatusDraft},
		{PostID: changed.ID, From: "video", To: "text", FromStatus: models.PostStatusDraft, ToStatus: models.PostStatusDraft},
		{PostID: scheduled.ID, From: "video", To: "text", FromStatus: models.PostStatusScheduled, ToStatus: models.PostStatusScheduled},
	}, time.Now().UTC())
	if err != nil || !slices.Equal(set, []string{ready.ID}) {
		t.Fatalf("changed = %v, %v", set, err)
	}
	got, _ := repo.GetByID(ctx, ready.ID)
	if got.PlatformPostType != "text" || got.Status != models.PostStatusDraft {
		t.Fatalf("post = %s %s", got.PlatformPostType, got.Status)
	}
}
