package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestAssetDelete_ScrubsDanglingReferences verifies that deleting an asset also
// removes its id from every campaign.asset_ids and post.used_asset_ids (CON-214),
// so a deleted asset can never linger as a dangling reference that later
// hard-fails content generation ("asset %q not found").
func TestAssetDelete_ScrubsDanglingReferences(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	now := time.Now().UTC()

	const delID, keepID = "asset-del", "asset-keep"

	asset := &models.Asset{
		ID:        delID,
		Title:     "To delete",
		Content:   "body",
		Status:    models.AssetStatusReady,
		TagIDs:    models.StringSlice{},
		CreatedBy: "user-1",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := db.NewInsert().Model(asset).Exec(ctx); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	campaign := &models.Campaign{
		ID:              "camp-1",
		Name:            "C",
		UseAssets:       true,
		AssetIDs:        models.StringSlice{delID, keepID},
		TargetPlatforms: models.CampaignPlatforms{},
		PublishingDays:  models.StringSlice{},
		TagIDs:          models.StringSlice{},
		Status:          models.StatusActive,
		CreatedBy:       "user-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := db.NewInsert().Model(campaign).Exec(ctx); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}

	post := &models.Post{
		ID:           "post-1",
		CampaignID:   "camp-1",
		Title:        "P",
		Content:      "body",
		MediaURLs:    models.StringSlice{},
		UsedAssetIDs: models.StringSlice{delID, keepID},
		Status:       models.PostStatusDraft,
		CTAType:      models.CTATypeNone,
		CreatedBy:    "user-1",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	repo := repository.NewAssetRepository(db, nil, nil)
	deleted, err := repo.Delete(ctx, delID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted {
		t.Fatal("expected deleted=true")
	}

	// The deleted id is scrubbed from both back-references; the surviving id stays.
	gotCampaign := new(models.Campaign)
	if err := db.NewSelect().Model(gotCampaign).Where("c.id = ?", "camp-1").Scan(ctx); err != nil {
		t.Fatalf("reload campaign: %v", err)
	}
	if got := []string(gotCampaign.AssetIDs); len(got) != 1 || got[0] != keepID {
		t.Errorf("campaign.AssetIDs = %v, want [%s]", got, keepID)
	}

	gotPost := new(models.Post)
	if err := db.NewSelect().Model(gotPost).Where("po.id = ?", "post-1").Scan(ctx); err != nil {
		t.Fatalf("reload post: %v", err)
	}
	if got := []string(gotPost.UsedAssetIDs); len(got) != 1 || got[0] != keepID {
		t.Errorf("post.UsedAssetIDs = %v, want [%s]", got, keepID)
	}
}

// TestAssetDelete_SystemContext_ScopesScrubToOwningTenant verifies that a
// system-context delete (no tenant in context, so the TenantScoped hooks add no
// predicate) only scrubs the deleted asset's own tenant. A second tenant whose
// campaign/post reference the same id must be left untouched — proving the
// jsonb-scrub updates are scoped to the owning tenant explicitly, not just by
// asset-id uniqueness.
func TestAssetDelete_SystemContext_ScopesScrubToOwningTenant(t *testing.T) {
	db := openMigratedDB(t)
	now := time.Now().UTC()

	const assetID, keepID = "asset-shared", "asset-keep"
	const tenantA, tenantB = "tenant-a", "tenant-b"

	ctxA := tenantctx.With(t.Context(), tenantA)
	ctxB := tenantctx.With(t.Context(), tenantB)

	seedCampaign := func(ctx context.Context, id string, assetIDs ...string) {
		t.Helper()
		c := &models.Campaign{
			ID:              id,
			Name:            "C",
			UseAssets:       true,
			AssetIDs:        models.StringSlice(assetIDs),
			TargetPlatforms: models.CampaignPlatforms{},
			PublishingDays:  models.StringSlice{},
			TagIDs:          models.StringSlice{},
			Status:          models.StatusActive,
			CreatedBy:       "user-1",
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if _, err := db.NewInsert().Model(c).Exec(ctx); err != nil {
			t.Fatalf("seed campaign %s: %v", id, err)
		}
	}
	seedPost := func(ctx context.Context, id string, usedAssetIDs ...string) {
		t.Helper()
		p := &models.Post{
			ID:           id,
			CampaignID:   "camp-" + id,
			Title:        "P",
			Content:      "body",
			MediaURLs:    models.StringSlice{},
			UsedAssetIDs: models.StringSlice(usedAssetIDs),
			Status:       models.PostStatusDraft,
			CTAType:      models.CTATypeNone,
			CreatedBy:    "user-1",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if _, err := db.NewInsert().Model(p).Exec(ctx); err != nil {
			t.Fatalf("seed post %s: %v", id, err)
		}
	}

	// The deleted asset lives in tenant A, which references it. Tenant B also
	// references the same id (a crafted cross-tenant collision) and must survive.
	asset := &models.Asset{
		ID:        assetID,
		Title:     "Shared",
		Content:   "body",
		Status:    models.AssetStatusReady,
		TagIDs:    models.StringSlice{},
		CreatedBy: "user-1",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := db.NewInsert().Model(asset).Exec(ctxA); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	seedCampaign(ctxA, "camp-a", assetID, keepID)
	seedPost(ctxA, "a", assetID, keepID)
	seedCampaign(ctxB, "camp-b", assetID, keepID)
	seedPost(ctxB, "b", assetID, keepID)

	repo := repository.NewAssetRepository(db, nil, nil)
	sys := tenantctx.WithSystem(t.Context())
	deleted, err := repo.Delete(sys, assetID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted {
		t.Fatal("expected deleted=true")
	}

	assertAssetIDs := func(ctx context.Context, id string, want ...string) {
		t.Helper()
		c := new(models.Campaign)
		if err := db.NewSelect().Model(c).Where("c.id = ?", id).Scan(ctx); err != nil {
			t.Fatalf("reload campaign %s: %v", id, err)
		}
		if got := []string(c.AssetIDs); !equalStrings(got, want) {
			t.Errorf("campaign %s AssetIDs = %v, want %v", id, got, want)
		}
	}
	assertUsedAssetIDs := func(ctx context.Context, id string, want ...string) {
		t.Helper()
		p := new(models.Post)
		if err := db.NewSelect().Model(p).Where("po.id = ?", id).Scan(ctx); err != nil {
			t.Fatalf("reload post %s: %v", id, err)
		}
		if got := []string(p.UsedAssetIDs); !equalStrings(got, want) {
			t.Errorf("post %s UsedAssetIDs = %v, want %v", id, got, want)
		}
	}

	// Tenant A (the owner) is scrubbed; tenant B keeps its reference intact.
	assertAssetIDs(ctxA, "camp-a", keepID)
	assertUsedAssetIDs(ctxA, "a", keepID)
	assertAssetIDs(ctxB, "camp-b", assetID, keepID)
	assertUsedAssetIDs(ctxB, "b", assetID, keepID)
}

// TestAssetSetImageResult_KeepsMidRunEdit: the vision description replaces the
// content only while it still holds the value from when the run started, so a
// description the user edited mid-run survives (CON-312).
func TestAssetSetImageResult_KeepsMidRunEdit(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewAssetRepository(db, nil, nil)
	imgType := models.AssetTypeImage
	seed := func(id, content string) {
		t.Helper()
		if _, err := db.NewInsert().Model(&models.Asset{
			ID: id, Title: id, Content: content, Status: models.AssetStatusProcessing,
			Type: &imgType, TagIDs: models.StringSlice{}, CreatedBy: "user-1",
		}).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	content := func(id string) string {
		t.Helper()
		var c string
		if err := db.NewSelect().Model((*models.Asset)(nil)).Column("content").Where("id = ?", id).Scan(ctx, &c); err != nil {
			t.Fatalf("reload %s: %v", id, err)
		}
		return c
	}

	seed("img-untouched", "")
	if err := repo.SetImageResult(ctx, "img-untouched", "", "generated", "", false); err != nil {
		t.Fatal(err)
	}
	if got := content("img-untouched"); got != "generated" {
		t.Fatalf("untouched asset: content = %q, want the generated description", got)
	}

	seed("img-edited", "user's words") // edited after the run read "" as prevContent
	if err := repo.SetImageResult(ctx, "img-edited", "", "generated", "", false); err != nil {
		t.Fatal(err)
	}
	if got := content("img-edited"); got != "user's words" {
		t.Fatalf("edited asset: content = %q, want the user's edit kept", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
