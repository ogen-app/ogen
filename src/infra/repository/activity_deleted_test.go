package repository_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// DeletedPostsBetween lists the tenant's post deletes in the window newest
// first, narrowed to a campaign by the campaign_id its payload names; other
// tenants' deletes and other event types don't count.
func TestActivityRepository_DeletedPostsBetween(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	other := tenantctx.With(context.Background(), "tenant-other")
	repo := repository.NewActivityRepository(db)

	t0 := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	seed := func(ctx context.Context, id, typ, post, campaign string, at time.Time) {
		t.Helper()
		e := &models.ActivityEvent{
			ID: id, Category: "post", Type: typ, EntityType: "post", EntityID: post, Source: "api",
			Payload: models.JSONMap{"campaign_id": campaign}, OccurredAt: at,
		}
		if _, err := db.NewInsert().Model(e).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed(ctx, "a1", repository.ActivityPostDeleted, "p1", "c1", t0)
	seed(ctx, "a2", repository.ActivityPostDeleted, "p2", "c2", t0.Add(time.Hour))
	seed(ctx, "a3", repository.ActivityPostDeleted, "p3", "c1", t0.Add(-48*time.Hour)) // before the window
	seed(ctx, "a4", "post_created", "p4", "c1", t0.Add(2*time.Hour))
	seed(other, "a5", repository.ActivityPostDeleted, "p5", "c1", t0)

	from, to := t0.Add(-time.Hour), t0.Add(24*time.Hour)
	ids := func(rows []repository.DeletedPost) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.PostID)
		}
		return out
	}

	all, err := repo.DeletedPostsBetween(ctx, from, to, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); !slices.Equal(got, []string{"p2", "p1"}) {
		t.Fatalf("deleted = %v, want p2 then p1", got)
	}
	if !all[1].At.Equal(t0) {
		t.Fatalf("p1 at %v, want %v", all[1].At, t0)
	}

	inCampaign, err := repo.DeletedPostsBetween(ctx, from, to, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(inCampaign); !slices.Equal(got, []string{"p1"}) {
		t.Fatalf("campaign c1 deleted = %v, want p1", got)
	}

	capped, err := repo.DeletedPostsBetween(ctx, from, to, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(capped); !slices.Equal(got, []string{"p2"}) {
		t.Fatalf("capped = %v, want the newest", got)
	}
}
