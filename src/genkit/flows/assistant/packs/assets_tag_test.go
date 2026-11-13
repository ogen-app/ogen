package packs

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// A tagging says which tags it added and removed, by name, and what
// changed on each of the first few assets: an asset that already had the
// tag is listed unchanged, and only the changed ones are undone.
func TestTagAssets_SaysWhatChanged(t *testing.T) {
	db, actions := undoStore(t)
	tags := repository.NewTagRepository(db)
	assets := repository.NewAssetRepository(db, tags, repository.NewAssetFileRepository(db))
	d := Deps{Assets: assets, Tags: tags, Actions: actions}
	ctx, _ := turn(t, workspace, nil)
	for _, m := range []any{
		&models.Account{ID: "acc-1", Email: "jane@example.com", Name: "Jane"},
		&models.User{ID: actor(ctx), AccountID: "acc-1", TenantID: "tenant-1", Name: "Jane", Email: "jane@example.com", Role: models.RoleOwner},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	spa, err := tags.FindOrCreateByName(ctx, "spa", "", actor(ctx))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var ids []string
	for i := range 7 {
		a := &models.Asset{ID: fmt.Sprintf("a%d", i), Title: fmt.Sprintf("Asset %d", i), TagIDs: models.StringSlice{},
			Content: "body", Status: models.AssetStatusReady, CreatedBy: actor(ctx), CreatedAt: now, UpdatedAt: now}
		if i == 1 {
			a.TagIDs = models.StringSlice{spa.ID}
		}
		if err := assets.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}

	out, err := tagAssets(ctx, d, TagAssetsInput{AssetIDs: ids, Add: []string{"Spa"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Tagged != 7 || out.Changed != 6 || !slices.Equal(out.Add, []string{"spa"}) || out.UndoID == "" {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Assets) != taggedShown || out.More != 2 {
		t.Fatalf("listed %d, more %d", len(out.Assets), out.More)
	}
	for _, a := range out.Assets {
		had := a.ID == "a1"
		if a.Title == "" || a.Removed != nil || had != (a.Added == nil) || !had && !slices.Equal(a.Added, []string{"spa"}) {
			t.Fatalf("%s = %+v; want spa added unless it had it", a.ID, a)
		}
	}

	out, err = tagAssets(ctx, d, TagAssetsInput{AssetIDs: []string{"a1"}, Remove: []string{"spa"}})
	if err != nil || !slices.Equal(out.Remove, []string{"spa"}) || len(out.Assets) != 1 || !slices.Equal(out.Assets[0].Removed, []string{"spa"}) {
		t.Fatalf("untag = %+v, %v", out, err)
	}
}
