package repository_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestAssetChunks_ReadsSkipEmbedding(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	now := time.Now().UTC()

	const assetID = "asset-chunks"
	asset := &models.Asset{
		ID: assetID, Title: "Doc", Content: "body", Status: models.AssetStatusReady,
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := db.NewInsert().Model(asset).Exec(ctx); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	repo := repository.NewAssetChunksRepository(db)

	n, first, err := repo.PreviewByAssetID(ctx, assetID)
	if err != nil {
		t.Fatalf("preview of empty asset: %v", err)
	}
	if n != 0 || first != "" {
		t.Fatalf("preview of empty asset = (%d, %q), want (0, \"\")", n, first)
	}

	vec := make([]float32, 3072)
	vec[0] = 1
	chunks := make([]models.AssetChunk, 3)
	ids := make([]string, len(chunks))
	for i := range chunks {
		ids[i] = fmt.Sprintf("%s:%d", assetID, i)
		chunks[i] = models.AssetChunk{
			ID: ids[i], AssetID: assetID, ChunkIndex: i, Content: fmt.Sprintf("chunk %d", i),
			TokenCount: 2, Embedding: pgvector.NewHalfVector(vec), Model: "m",
		}
	}
	// Insert out of order so the preview has to pick chunk_index 0, not the
	// first row written.
	chunks[0], chunks[2] = chunks[2], chunks[0]
	if err := repo.UpsertChunks(ctx, assetID, chunks); err != nil {
		t.Fatalf("upsert chunks: %v", err)
	}

	n, first, err = repo.PreviewByAssetID(ctx, assetID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if n != 3 || first != "chunk 0" {
		t.Fatalf("preview = (%d, %q), want (3, \"chunk 0\")", n, first)
	}

	byAsset, err := repo.GetByAssetID(ctx, assetID)
	if err != nil {
		t.Fatalf("get by asset: %v", err)
	}
	byIDs, err := repo.GetByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("get by ids: %v", err)
	}
	similar, err := repo.SearchSimilar(ctx, pgvector.NewHalfVector(vec), []string{assetID}, 0.5, 0)
	if err != nil {
		t.Fatalf("search similar: %v", err)
	}
	for name, got := range map[string][]models.AssetChunk{"GetByAssetID": byAsset, "GetByIDs": byIDs, "SearchSimilar": similar} {
		if len(got) != 3 {
			t.Fatalf("%s returned %d chunks, want 3", name, len(got))
		}
		for _, c := range got {
			if len(c.Embedding.Slice()) != 0 {
				t.Errorf("%s loaded the embedding of %s", name, c.ID)
			}
			if c.Content == "" {
				t.Errorf("%s returned %s without content", name, c.ID)
			}
		}
	}
}
