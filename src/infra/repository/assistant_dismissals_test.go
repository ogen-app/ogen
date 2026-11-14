package repository_test

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

func TestAssistantDismissals(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantDismissalRepository(db)
	ctx := tenantCtx()

	for range 2 {
		if err := repo.Dismiss(ctx, models.AssistantScopeCampaign, "c1", "gap:abc", "u1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Dismiss(ctx, models.AssistantScopeCampaign, "c2", "gap:def", "u1"); err != nil {
		t.Fatal(err)
	}
	keys, err := repo.Keys(ctx, models.AssistantScopeCampaign, "c1")
	if err != nil || len(keys) != 1 || !keys["gap:abc"] {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	other := tenantctx.With(context.Background(), "tenant-other")
	if keys, err := repo.Keys(other, models.AssistantScopeCampaign, "c1"); err != nil || len(keys) != 0 {
		t.Fatalf("another tenant read %v (%v)", keys, err)
	}

	list, err := repo.List(ctx, models.AssistantScopeCampaign, "c1")
	if err != nil || len(list) != 1 || list[0].FindingKey != "gap:abc" || list[0].CreatedBy != "u1" || list[0].CreatedAt.IsZero() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// Another tenant can't restore it; the tenant can, once.
	if ok, err := repo.Restore(other, models.AssistantScopeCampaign, "c1", "gap:abc"); err != nil || ok {
		t.Fatalf("another tenant restored: %v, %v", ok, err)
	}
	for i, want := range []bool{true, false} {
		if ok, err := repo.Restore(ctx, models.AssistantScopeCampaign, "c1", "gap:abc"); err != nil || ok != want {
			t.Fatalf("restore %d = %v, %v", i, ok, err)
		}
	}
	if keys, err := repo.Keys(ctx, models.AssistantScopeCampaign, "c1"); err != nil || len(keys) != 0 {
		t.Fatalf("keys after restore = %v, %v", keys, err)
	}
	if keys, err := repo.Keys(ctx, models.AssistantScopeCampaign, "c2"); err != nil || !keys["gap:def"] {
		t.Fatalf("restore reached another campaign: %v, %v", keys, err)
	}
}
