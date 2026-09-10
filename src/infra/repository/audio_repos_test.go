package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// TestAudioReposRoundTrip exercises the CON-282 processing-state repos against a
// real Postgres: it validates the bun tags (incl. the reserved-ish `index`
// column on audio_segments), tenant scoping, ResetFailed, and the
// replace-for-segment idempotency the job relies on.
func TestAudioReposRoundTrip(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	now := time.Now().UTC()

	// FK parent: the extraction/segment/utterance rows reference this asset.
	asset := &models.Asset{
		ID: "aud-1", Title: "podcast", Content: "", Status: models.AssetStatusPending,
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := db.NewInsert().Model(asset).Exec(ctx); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	extRepo := repository.NewAudioExtractionRepository(db)
	ext := &models.AudioExtraction{
		ID: "e1", AssetID: "aud-1", RunKey: "run-1",
		Status: models.AudioExtractionStatusPending, TranscribeModel: "gemini-2.5-flash",
	}
	if err := extRepo.Create(ctx, ext); err != nil {
		t.Fatalf("create extraction: %v", err)
	}
	got, err := extRepo.GetByAssetAndRunKey(ctx, "aud-1", "run-1")
	if err != nil || got.ID != "e1" {
		t.Fatalf("get extraction: %v %+v", err, got)
	}
	ext.Status = models.AudioExtractionStatusTranscribing
	ext.CostMicros = 42
	ext.PriceVersion = "v1"
	if err := extRepo.Update(ctx, ext); err != nil {
		t.Fatalf("update extraction: %v", err)
	}
	latest, err := extRepo.GetLatestByAsset(ctx, "aud-1")
	if err != nil || latest.Status != models.AudioExtractionStatusTranscribing || latest.CostMicros != 42 {
		t.Fatalf("latest extraction wrong: %v %+v", err, latest)
	}

	segRepo := repository.NewAudioSegmentRepository(db)
	if err := segRepo.CreateMany(ctx, []models.AudioSegment{
		{ID: "s0", ExtractionID: "e1", AssetID: "aud-1", Index: 0, StartMs: 0, EndMs: 1000, Status: models.AudioSegmentStatusDone},
		{ID: "s1", ExtractionID: "e1", AssetID: "aud-1", Index: 1, StartMs: 900, EndMs: 2000, Status: models.AudioSegmentStatusFailed},
	}); err != nil {
		t.Fatalf("create segments: %v", err)
	}
	// Idempotent re-create (ON CONFLICT DO NOTHING) — must not error or duplicate.
	if err := segRepo.CreateMany(ctx, []models.AudioSegment{
		{ID: "s0-dup", ExtractionID: "e1", AssetID: "aud-1", Index: 0, StartMs: 0, EndMs: 1000, Status: models.AudioSegmentStatusPending},
	}); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	segs, err := segRepo.ListByExtraction(ctx, "e1")
	if err != nil || len(segs) != 2 || segs[0].Index != 0 || segs[1].Index != 1 {
		t.Fatalf("list segments wrong: %v %+v", err, segs)
	}
	reset, err := segRepo.ResetFailed(ctx, "e1")
	if err != nil || reset != 1 {
		t.Fatalf("reset failed: %v reset=%d", err, reset)
	}
	segs, _ = segRepo.ListByExtraction(ctx, "e1")
	if segs[1].Status != models.AudioSegmentStatusPending {
		t.Fatalf("failed segment not reset to pending: %+v", segs[1])
	}

	uttRepo := repository.NewUtteranceRepository(db)
	if err := uttRepo.ReplaceForSegment(ctx, "s0", []models.Utterance{
		{ID: "u0", SegmentID: "s0", AssetID: "aud-1", Index: 0, StartMs: 0, EndMs: 500, Text: "hello", IsSpeech: true, Confidence: -1},
	}); err != nil {
		t.Fatalf("replace utterances: %v", err)
	}
	// Replace again with two — the old span is dropped (segment-retry idempotency).
	if err := uttRepo.ReplaceForSegment(ctx, "s0", []models.Utterance{
		{ID: "u1", SegmentID: "s0", AssetID: "aud-1", Index: 0, StartMs: 0, EndMs: 500, Text: "hello", IsSpeech: true, Confidence: -1},
		{ID: "u2", SegmentID: "s0", AssetID: "aud-1", Index: 1, StartMs: 500, EndMs: 1000, Text: "world", IsSpeech: true, Confidence: -1},
	}); err != nil {
		t.Fatalf("replace utterances 2: %v", err)
	}
	utts, err := uttRepo.ListByAsset(ctx, "aud-1")
	if err != nil || len(utts) != 2 || utts[0].Text != "hello" || utts[1].Text != "world" {
		t.Fatalf("list utterances wrong: %v %+v", err, utts)
	}
}
