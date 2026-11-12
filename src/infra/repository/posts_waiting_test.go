package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// NextScheduled is the first post waiting to go out after a moment, of
// either route; ScheduledCounts tallies the waiting ones by platform and
// account. Another tenant's posts are neither.
func TestPostRepository_Waiting(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	now := time.Date(2026, 4, 21, 9, 0, 0, 0, time.UTC)
	seed := func(ctx context.Context, id, campaign, platform, account string, status models.PostStatus, at time.Time) {
		t.Helper()
		post := &models.Post{
			ID: id, CampaignID: campaign, PlatformID: platform, SocialAccountID: account, Title: "Post " + id,
			Content: "Body", MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
			Status: status, ScheduledAt: &at, CTAType: models.CTATypeNone, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
		}
		if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	acct := &models.SocialAccount{ID: "acc-li", Platform: "linkedin", ProfileID: "prof", Username: "dan", RawJSON: "{}", IsActive: true, ConnectedAt: now, LastSyncedAt: now}
	if _, err := db.NewInsert().Model(acct).Exec(ctx); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	seed(ctx, "past", "c1", "ig", "", models.PostStatusScheduled, now.Add(-time.Hour))
	seed(ctx, "manual", "c2", "li", "acc-li", models.PostStatusScheduledForManualPublish, now.Add(2*time.Hour))
	seed(ctx, "auto", "c1", "ig", "", models.PostStatusScheduled, now.Add(5*time.Hour))
	seed(ctx, "draft", "c1", "ig", "", models.PostStatusDraft, now.Add(time.Hour))
	seed(tenantctx.With(context.Background(), "other-tenant"), "theirs", "c1", "ig", "", models.PostStatusScheduled, now.Add(time.Minute))

	next, err := repo.NextScheduled(ctx, now, "")
	if err != nil || next == nil || next.ID != "manual" || next.Title != "Post manual" || next.ScheduledAt == nil {
		t.Fatalf("next = %+v, %v; want the manual post, either route counting", next, err)
	}
	if next, err := repo.NextScheduled(ctx, now, "c1"); err != nil || next == nil || next.ID != "auto" {
		t.Fatalf("next in c1 = %+v, %v", next, err)
	}
	if next, err := repo.NextScheduled(ctx, now.Add(6*time.Hour), ""); err != nil || next != nil {
		t.Fatalf("next after all = %+v, %v; want none", next, err)
	}

	counts, err := repo.ScheduledCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []repository.ScheduledCount{
		{PlatformID: "ig", Status: models.PostStatusScheduled, Count: 2},
		{PlatformID: "li", AccountID: "acc-li", Status: models.PostStatusScheduledForManualPublish, Count: 1},
	}
	if len(counts) != len(want) || counts[0] != want[0] || counts[1] != want[1] {
		t.Fatalf("counts = %+v; want %+v", counts, want)
	}
}
