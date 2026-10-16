package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestAssetChunkStages_EmbedThenCommit(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	now := time.Now().UTC()
	const assetID = "asset-stages"
	if _, err := db.NewInsert().Model(&models.Asset{
		ID: assetID, Title: "Doc", Content: "body", Status: models.AssetStatusProcessing,
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	stages := repository.NewAssetChunkStagesRepository(db)
	chunks := repository.NewAssetChunksRepository(db)
	vec := make([]float32, 3072)
	vec[0] = 1

	// An earlier run of the asset is dropped when a new one begins.
	if err := stages.Begin(ctx, "run-old", assetID, []models.AssetChunkStage{{ChunkIndex: 0, Content: "old", TokenCount: 1}}); err != nil {
		t.Fatal(err)
	}
	label := "Slide 2"
	if err := stages.Begin(ctx, "run-1", assetID, []models.AssetChunkStage{
		{ChunkIndex: 0, Content: "zero", TokenCount: 3},
		{ChunkIndex: 1, Content: "one", TokenCount: 4, SourceLabel: &label},
		{ChunkIndex: 2, Content: "two", TokenCount: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if old, _ := stages.Pending(ctx, "run-old", 10); len(old) != 0 {
		t.Fatalf("earlier run kept %d stages", len(old))
	}
	pending, err := stages.Pending(ctx, "run-1", 2)
	if err != nil || len(pending) != 2 || pending[0].ChunkIndex != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if err := stages.SetEmbeddings(ctx, "run-1", "m", map[int][]float32{0: vec, 1: vec}); err != nil {
		t.Fatal(err)
	}
	if err := stages.MarkFailed(ctx, "run-1", []int{2}); err != nil {
		t.Fatal(err)
	}
	if left, _ := stages.Pending(ctx, "run-1", 10); len(left) != 0 {
		t.Fatalf("still pending: %+v", left)
	}
	if rows, err := stages.Rows(ctx, "run-1", []int{1}); err != nil || len(rows) != 1 || rows[0].Content != "one" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}

	st, err := stages.Commit(ctx, "run-1", assetID, false)
	if err != nil {
		t.Fatal(err)
	}
	if st != (repository.ChunkStageStats{Attempts: 3, Embedded: 2, Failures: 1, Tokens: 7}) {
		t.Errorf("stats = %+v", st)
	}
	got, err := chunks.GetByAssetID(ctx, assetID)
	if err != nil || len(got) != 2 || got[0].ID != assetID+":0" || got[1].SourceLabel == nil || *got[1].SourceLabel != label || got[1].Model != "m" {
		t.Fatalf("chunks = %+v, %v", got, err)
	}
	if rows, _ := stages.Rows(ctx, "run-1", []int{0, 1, 2}); len(rows) != 0 {
		t.Errorf("committed run kept %d stages", len(rows))
	}

	// A run that embedded nothing leaves the chunks as they were.
	if err := stages.Begin(ctx, "run-2", assetID, []models.AssetChunkStage{{ChunkIndex: 0, Content: "x", TokenCount: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := stages.MarkFailed(ctx, "run-2", []int{0}); err != nil {
		t.Fatal(err)
	}
	if _, err := stages.Commit(ctx, "run-2", assetID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := chunks.GetByAssetID(ctx, assetID); len(got) != 2 {
		t.Errorf("a failed run replaced the chunks: %d left", len(got))
	}
	// An empty run with clearStale clears them.
	if err := stages.Begin(ctx, "run-3", assetID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := stages.Commit(ctx, "run-3", assetID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := chunks.GetByAssetID(ctx, assetID); len(got) != 0 {
		t.Errorf("clearStale left %d chunks", len(got))
	}
}
