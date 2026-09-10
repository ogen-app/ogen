package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// insertSummaryPost seeds a post in the given tenant context (the TenantScoped
// hook stamps tenant_id from the context on insert). FK enforcement is bypassed
// by openMigratedDB, so a synthetic campaign_id needs no seeded campaign.
func insertSummaryPost(t *testing.T, db *bun.DB, ctx context.Context, id, campaignID string, status models.PostStatus, createdAt time.Time) {
	t.Helper()
	post := &models.Post{
		ID:           id,
		CampaignID:   campaignID,
		Title:        "Title " + id,
		Content:      "Body " + id, // must be dropped by the projection
		MediaURLs:    models.StringSlice{},
		UsedAssetIDs: models.StringSlice{},
		Status:       status,
		CTAType:      models.CTATypeNone,
		CreatedBy:    "user-1",
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatalf("seed post %s: %v", id, err)
	}
}

// TestListSummaryProjections_GroupsProjectsAndDropsHeavyColumns verifies the
// CON-152 batched read returns every post ordered by campaign then created_at,
// with the heavy content column dropped and no relations hydrated.
func TestListSummaryProjections_GroupsProjectsAndDropsHeavyColumns(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Insert camp-b before camp-a, and a2 before a1, to prove ORDER BY (not
	// insertion order) drives the result.
	insertSummaryPost(t, db, ctx, "b1", "camp-b", models.PostStatusFailed, t0.Add(3*time.Hour))
	insertSummaryPost(t, db, ctx, "a2", "camp-a", models.PostStatusScheduled, t0.Add(2*time.Hour))
	insertSummaryPost(t, db, ctx, "a1", "camp-a", models.PostStatusDraft, t0.Add(1*time.Hour))

	posts, err := repo.ListSummaryProjections(ctx)
	if err != nil {
		t.Fatalf("list summary projections: %v", err)
	}
	if len(posts) != 3 {
		t.Fatalf("want 3 posts, got %d", len(posts))
	}

	// Ordered by campaign_id ASC, then created_at ASC.
	wantOrder := []string{"a1", "a2", "b1"}
	for i, id := range wantOrder {
		if posts[i].ID != id {
			t.Fatalf("order[%d] = %q, want %q (full: %+v)", i, posts[i].ID, id, posts)
		}
	}

	// Heavy column dropped; readiness fields present; relations not hydrated.
	for _, p := range posts {
		if p.Content != "" || p.Title != "" {
			t.Fatalf("projection should drop title/content, got title=%q content=%q", p.Title, p.Content)
		}
		if p.Campaign != nil || p.Platform != nil || p.CampaignTypePhase != nil {
			t.Fatalf("projection must not hydrate relations, got %+v", p)
		}
		if p.CampaignID == "" || p.Status == "" {
			t.Fatalf("readiness fields missing: %+v", p)
		}
	}
}

// TestListSummaryProjections_TenantScoped is the isolation guard: the read
// returns only the caller's tenant, and fails closed with no tenant in context.
func TestListSummaryProjections_TenantScoped(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostRepository(db)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctxDefault := tenantCtx()
	ctxOther := tenantctx.With(t.Context(), "tenant-other")

	insertSummaryPost(t, db, ctxDefault, "mine-1", "camp-a", models.PostStatusDraft, t0)
	insertSummaryPost(t, db, ctxOther, "theirs-1", "camp-z", models.PostStatusDraft, t0)

	got, err := repo.ListSummaryProjections(ctxDefault)
	if err != nil {
		t.Fatalf("scoped read: %v", err)
	}
	if len(got) != 1 || got[0].ID != "mine-1" {
		t.Fatalf("tenant leak: expected only [mine-1], got %+v", got)
	}

	// Fail-closed: no tenant in context must refuse to run (CON-97).
	if _, err := repo.ListSummaryProjections(t.Context()); err == nil {
		t.Fatalf("expected fail-closed error for an unscoped read, got nil")
	}
}
