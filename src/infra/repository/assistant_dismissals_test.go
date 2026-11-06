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
}
