package repository_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestTenantRepositoryCRUD(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewTenantRepository(db)
	ctx := t.Context()

	tn := &models.Tenant{ID: "tn-1", Name: "Acme", Slug: "acme", TierID: models.DefaultTierID}
	if err := repo.Create(ctx, tn); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, "tn-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Name != "Acme" || got.Slug != "acme" {
		t.Fatalf("unexpected tenant: %+v", got)
	}

	bySlug, err := repo.GetBySlug(ctx, "acme")
	if err != nil || bySlug.ID != "tn-1" {
		t.Fatalf("get by slug: %v / %+v", err, bySlug)
	}

	got.Name = "Acme Inc"
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, err := repo.GetByID(ctx, "tn-1")
	if err != nil {
		t.Fatalf("reload after update: %v", err)
	}
	if reloaded.Name != "Acme Inc" {
		t.Fatalf("update not persisted: %+v", reloaded)
	}

	// The default tenant from the foundation migration must exist.
	def, err := repo.GetByID(ctx, models.DefaultTenantID)
	if err != nil {
		t.Fatalf("default tenant missing: %v", err)
	}
	if def.Slug != "default" {
		t.Fatalf("unexpected default tenant: %+v", def)
	}

	// Missing tenant → sql.ErrNoRows.
	if _, err := repo.GetByID(ctx, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
}
