package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

func TestAssistantActions_UndoOnce(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantActionRepository(db)
	ctx := tenantCtx()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	a := &models.AssistantAction{
		ID: id, ScopeKind: models.AssistantScopeCampaign, ScopeID: "camp-1",
		Kind: models.AssistantActionCampaignDates, EntityID: "camp-1",
		Before:    models.JSONMap{"startDate": nil, "endDate": "2026-06-30T00:00:00Z"},
		After:     models.JSONMap{"startDate": "2026-06-01T00:00:00Z", "endDate": "2026-07-31T00:00:00Z"},
		CreatedBy: "user-1",
	}
	if err := repo.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before["startDate"] != nil || got.After["endDate"] != "2026-07-31T00:00:00Z" || got.UndoneAt != nil {
		t.Fatalf("stored action = %+v", got)
	}

	if ok, err := repo.MarkUndone(ctx, id, "user-1", now); err != nil || !ok {
		t.Fatalf("first undo = %t, %v", ok, err)
	}
	if ok, err := repo.MarkUndone(ctx, id, "user-2", now); err != nil || ok {
		t.Fatalf("second undo = %t, %v, want false", ok, err)
	}
	got, err = repo.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.UndoneAt == nil || got.UndoneBy != "user-1" {
		t.Fatalf("undone action = %+v", got)
	}

	// Another tenant can neither read nor undo it.
	other := tenantctx.With(context.Background(), "tenant-other")
	if _, err := repo.GetByID(other, id); err == nil {
		t.Fatal("another tenant read the action")
	}
}
