package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// CountDecided counts the caller's ideas decided inside the window, in a
// campaign when one is named.
func TestIdeaRepository_CountDecided(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewIdeaRepository(db)

	lo := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	hi := lo.AddDate(0, 0, 7)
	yes := models.IdeaVerdictYes
	seed := func(ctx context.Context, id string, campaign *string, decided *time.Time) {
		t.Helper()
		idea := &models.Idea{ID: id, Title: id, CampaignID: campaign, Origin: models.IdeaOriginManual, CreatedAt: lo, UpdatedAt: lo}
		if decided != nil {
			idea.Verdict, idea.DecidedAt, idea.DecidedBy = &yes, decided, new("u1")
		}
		if _, err := db.NewInsert().Model(idea).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed(ctx, "in-ws", nil, new(lo.Add(time.Hour)))
	seed(ctx, "in-c1", new("c1"), new(hi.Add(-time.Minute)))
	seed(ctx, "at-end", nil, &hi)
	seed(ctx, "before", nil, new(lo.Add(-time.Minute)))
	seed(ctx, "undecided", nil, nil)
	seed(tenantctx.With(context.Background(), "other-tenant"), "theirs", nil, new(lo.Add(time.Hour)))

	if n, err := repo.CountDecided(ctx, lo, hi, ""); err != nil || n != 2 {
		t.Fatalf("decided = %d, %v; want the two inside, end exclusive", n, err)
	}
	if n, err := repo.CountDecided(ctx, lo, hi, "c1"); err != nil || n != 1 {
		t.Fatalf("decided in c1 = %d, %v", n, err)
	}
}
