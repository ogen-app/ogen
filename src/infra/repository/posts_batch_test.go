package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// TestPostUpdateScheduledAtBatch verifies the CON-115 batch update writes the
// scheduled_at column (and leaves the rest of the row intact).
func TestPostUpdateScheduledAtBatch(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	id, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	orig := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	post := &models.Post{
		ID:           id,
		CampaignID:   "camp-1",
		Title:        "Draft",
		Content:      "body",
		MediaURLs:    models.StringSlice{},
		UsedAssetIDs: models.StringSlice{},
		Status:       models.PostStatusDraft,
		CTAType:      models.CTATypeNone,
		CreatedBy:    "user-1",
		ScheduledAt:  &orig,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := repo.Create(ctx, post); err != nil {
		t.Fatalf("create: %v", err)
	}

	newAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	post.ScheduledAt = &newAt
	if err := repo.UpdateScheduledAtBatch(ctx, []*models.Post{post}); err != nil {
		t.Fatalf("batch update: %v", err)
	}

	got, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ScheduledAt == nil || !got.ScheduledAt.Equal(newAt) {
		t.Fatalf("scheduled_at = %v, want %v", got.ScheduledAt, newAt)
	}
	if got.Title != "Draft" || got.Status != models.PostStatusDraft {
		t.Fatalf("other columns clobbered: title=%q status=%q", got.Title, got.Status)
	}

	// A planned post is redistributable too.
	plannedID, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	planned := *post
	planned.ID, planned.Content, planned.Status = plannedID, "", models.PostStatusPlanned
	if err := repo.Create(ctx, &planned); err != nil {
		t.Fatalf("create planned: %v", err)
	}
	if err := repo.UpdateScheduledAtBatch(ctx, []*models.Post{&planned}); err != nil {
		t.Fatalf("batch update of a planned post: %v", err)
	}

	// Empty batch is a no-op.
	if err := repo.UpdateScheduledAtBatch(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}
