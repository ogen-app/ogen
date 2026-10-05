package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestAssetOriginRoundTrip(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db))
	now := time.Now().UTC()

	seed := func(id, origin string, ref *models.AssetOriginRef) {
		t.Helper()
		a := &models.Asset{
			ID: id, Title: id, Status: models.AssetStatusReady, TagIDs: models.StringSlice{},
			Origin: origin, OriginRef: ref, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
		}
		if _, err := db.NewInsert().Model(a).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("from-figma", models.AssetOriginFigma, &models.AssetOriginRef{NodeID: "12:345", NodeName: "Hero", FileName: "Launch"})
	seed("uploaded", "", nil)

	got, err := repo.GetByID(ctx, "from-figma")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != models.AssetOriginFigma || got.OriginRef == nil ||
		got.OriginRef.NodeID != "12:345" || got.OriginRef.NodeName != "Hero" || got.OriginRef.FileName != "Launch" {
		t.Fatalf("figma provenance = %q %+v", got.Origin, got.OriginRef)
	}

	got, err = repo.GetByID(ctx, "uploaded")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "" || got.OriginRef != nil {
		t.Fatalf("upload provenance = %q %+v, want empty", got.Origin, got.OriginRef)
	}

	if _, err := db.NewInsert().Model(&models.Asset{
		ID: "bad", Title: "bad", Status: models.AssetStatusReady, TagIDs: models.StringSlice{},
		Origin: "canva", CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx); err == nil {
		t.Fatal("an unknown origin must violate the CHECK")
	}
}
