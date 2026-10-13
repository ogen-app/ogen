package repository_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// TestAssetFileRepository_SetDurationSurvivesUpsert: SetDuration is the only
// writer of duration_ms, so a later Upsert of the same file (from a pipeline
// that knows nothing about duration) keeps it.
func TestAssetFileRepository_SetDurationSurvivesUpsert(t *testing.T) {
	f := newAnalysisFixture(t)
	files := repository.NewAssetFileRepository(f.db)
	f.seedAsset(t, "rec")

	file := &models.AssetFile{
		ID: "file-rec", AssetID: "rec", OriginalName: "call.mp3", MimeType: "audio/mpeg",
		SizeBytes: 1024, S3Key: "assets/rec/original.mp3",
	}
	if err := files.Upsert(f.ctx, file); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := files.SetDuration(f.ctx, "rec", 1_080_000); err != nil {
		t.Fatalf("set duration: %v", err)
	}
	file.SizeBytes = 2048
	if err := files.Upsert(f.ctx, file); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got, err := files.GetByAssetID(f.ctx, "rec")
	if err != nil {
		t.Fatal(err)
	}
	if got.DurationMs != 1_080_000 || got.SizeBytes != 2048 {
		t.Fatalf("file = duration %d size %d, want 1080000 and 2048", got.DurationMs, got.SizeBytes)
	}
}
