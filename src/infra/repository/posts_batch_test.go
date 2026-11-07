package repository_test

import (
	"slices"
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
	planned.ScheduledAt = &orig
	if err := repo.Create(ctx, &planned); err != nil {
		t.Fatalf("create planned: %v", err)
	}
	planned.ScheduledAt = &newAt
	if err := repo.UpdateScheduledAtBatch(ctx, []*models.Post{&planned}); err != nil {
		t.Fatalf("batch update of a planned post: %v", err)
	}
	got, err = repo.GetByID(ctx, plannedID)
	if err != nil {
		t.Fatalf("get planned: %v", err)
	}
	if got.ScheduledAt == nil || !got.ScheduledAt.Equal(newAt) {
		t.Fatalf("planned scheduled_at = %v, want %v", got.ScheduledAt, newAt)
	}

	// Empty batch is a no-op.
	if err := repo.UpdateScheduledAtBatch(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

// TestPostMoveScheduledAt verifies a move applies only while the post still
// has the schedule it was planned from, and skips the rest of the batch
// rather than failing it.
func TestPostMoveScheduledAt(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	mk := func(at *time.Time, status models.PostStatus) *models.Post {
		id, err := models.NewID()
		if err != nil {
			t.Fatal(err)
		}
		p := &models.Post{ID: id, CampaignID: "camp-1", Title: "Draft", MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
			Status: status, CTAType: models.CTATypeNone, CreatedBy: "user-1", ScheduledAt: at, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		return p
	}
	planned := time.Date(2026, 1, 5, 17, 0, 0, 0, time.UTC)
	edited := time.Date(2026, 1, 5, 20, 30, 0, 0, time.UTC)
	same := mk(&planned, models.PostStatusDraft)
	undated := mk(nil, models.PostStatusPlanned)
	retimed := mk(&edited, models.PostStatusDraft)        // edited the same day since
	scheduled := mk(&planned, models.PostStatusScheduled) // committed since

	to := time.Date(2026, 3, 20, 17, 0, 0, 0, time.UTC)
	moved, err := repo.MoveScheduledAt(ctx, []repository.ScheduleMove{
		{PostID: same.ID, From: &planned, To: to},
		{PostID: undated.ID, To: to},
		{PostID: retimed.ID, From: &planned, To: to},
		{PostID: scheduled.ID, From: &planned, To: to},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	slices.Sort(moved)
	want := []string{same.ID, undated.ID}
	slices.Sort(want)
	if !slices.Equal(moved, want) {
		t.Fatalf("moved = %v, want %v", moved, want)
	}
	for p, at := range map[*models.Post]time.Time{same: to, undated: to, retimed: edited, scheduled: planned} {
		got, err := repo.GetByID(ctx, p.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ScheduledAt == nil || !got.ScheduledAt.Equal(at) {
			t.Fatalf("post %s scheduled_at = %v, want %v", p.ID, got.ScheduledAt, at)
		}
	}

	if moved, err := repo.MoveScheduledAt(ctx, nil, time.Now()); err != nil || len(moved) != 0 {
		t.Fatalf("empty batch: %v, %v", moved, err)
	}
}
