package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestMediaPreviews_TenantScoped verifies a preview recorded in one tenant is
// invisible to another holding the same image, and that a lookup without a
// tenant fails closed.
func TestMediaPreviews_TenantScoped(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewMediaPreviewRepository(db)
	ctxA := tenantctx.With(t.Context(), "tenant-a")
	ctxB := tenantctx.With(t.Context(), "tenant-b")
	key := models.MediaPreviewKey{Source: models.MediaPreviewSourceImage, SourceRef: "sum-shared"}

	if err := repo.Create(ctxA, &models.MediaPreview{
		Source: key.Source, SourceRef: key.SourceRef, MaxLongEdge: 4096,
		S3Key: "t/tenant-a/media-previews/x", MimeType: "image/jpeg", Width: 10, Height: 10, SizeBytes: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.ListByKeys(ctxA, 4096, []models.MediaPreviewKey{key})
	if err != nil || got[key].S3Key != "t/tenant-a/media-previews/x" {
		t.Fatalf("tenant A lookup = %v, %v; want its preview", got, err)
	}
	got, err = repo.ListByKeys(ctxB, 4096, []models.MediaPreviewKey{key})
	if err != nil || len(got) != 0 {
		t.Fatalf("tenant B lookup = %v, %v; want nothing", got, err)
	}
	if _, err := repo.ListByKeys(context.Background(), 4096, []models.MediaPreviewKey{key}); !errors.Is(err, tenantctx.ErrNoTenant) {
		t.Fatalf("lookup without tenant err = %v, want ErrNoTenant", err)
	}
}
