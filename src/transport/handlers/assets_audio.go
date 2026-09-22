package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
)

// maxAudioUploadBytes caps a direct-to-storage audio upload (CON-282). Audio is
// large (an hour of speech is hundreds of MB), so the bytes never pass through
// the API — the client PUTs to a presigned URL and the size is enforced at
// finalize via Head against the authoritative object.
const maxAudioUploadBytes = 5 << 30 // 5 GiB

// audioPresignTTL bounds the presigned PUT URL handed to the client, and the GET
// URL Head uses. Long enough for a large upload over a slow link.
const audioUploadPresignTTL = 30 * time.Minute

// audioUploadMIMEs is the CON-282 accept list: audio extensions audio-service
// transcribes, mapped to the MIME stored in asset_files.mime_type and bound on
// the presigned PUT. The extension is advisory routing only — audio-service
// sniffs the container authoritatively.
var audioUploadMIMEs = map[string]string{
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/opus",
	".flac": "audio/flac",
	".webm": "audio/webm",
	".aif":  "audio/aiff",
	".aiff": "audio/aiff",
}

// AudioIngestEnqueuer enqueues an audio-ingestion job in the caller's
// transaction (CON-282). Implemented by *queues.Enqueuer; a narrow interface
// here keeps the handler off the jobs package.
type AudioIngestEnqueuer interface {
	EnqueueProcessAudioTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error
}

// errNoFailedSegments aborts the Retry transaction when there is nothing to
// re-drive, so the reset/update roll back and the handler answers 409.
var errNoFailedSegments = errors.New("no failed segments to retry")

// AudioAssetsHandler serves the audio asset lifecycle (CON-282): presigned
// direct upload, ingestion trigger, and the extraction status/transcript/retry
// surface. It is a focused sibling of AssetsHandler (CON-291 split), registered
// on the same /api/content-bank/assets group. A nil audioJobs (no
// AUDIO_SERVICE_ADDR) makes the write endpoints fail fast with 409.
type AudioAssetsHandler struct {
	repo        repository.AssetRepository
	fileRepo    repository.AssetFileRepository
	extractions repository.AudioExtractionRepository
	segments    repository.AudioSegmentRepository
	utterances  repository.UtteranceRepository
	storage     storage.Storage
	db          *bun.DB
	audioJobs   AudioIngestEnqueuer
	auth        fiber.Handler
}

func NewAudioAssetsHandler(
	repo repository.AssetRepository,
	fileRepo repository.AssetFileRepository,
	extractions repository.AudioExtractionRepository,
	segments repository.AudioSegmentRepository,
	utterances repository.UtteranceRepository,
	store storage.Storage,
	db *bun.DB,
	audioJobs AudioIngestEnqueuer,
	auth fiber.Handler,
) *AudioAssetsHandler {
	return &AudioAssetsHandler{
		repo:        repo,
		fileRepo:    fileRepo,
		extractions: extractions,
		segments:    segments,
		utterances:  utterances,
		storage:     store,
		db:          db,
		audioJobs:   audioJobs,
		auth:        auth,
	}
}

func (h *AudioAssetsHandler) Register(app *fiber.App) {
	g := app.Group("/api/content-bank/assets")
	g.Post("/audio/presign", h.auth, h.Presign)
	g.Post("/audio/finalize", h.auth, h.Finalize)
	g.Post("/:id/audio/extract", h.auth, h.Extract)
	g.Get("/:id/audio", h.auth, h.Status)
	g.Get("/:id/audio/transcript", h.auth, h.Transcript)
	g.Post("/:id/audio/retry", h.auth, h.Retry)
	g.Post("/:id/audio/reextract", h.auth, h.Reextract)
}

// configured reports whether audio ingestion is wired (service + storage + db).
func (h *AudioAssetsHandler) configured() bool {
	return h.audioJobs != nil && h.storage != nil && h.db != nil
}

type presignAudioRequest struct {
	Filename    string `json:"filename"     validate:"required"`
	ContentType string `json:"content_type"`
}

type presignAudioResponse struct {
	Asset      *models.Asset     `json:"asset"`
	UploadURL  string            `json:"upload_url"`
	StorageKey string            `json:"storage_key"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
}

// Presign mints a pending AUDIO asset and returns a short-lived presigned PUT the
// client uploads the bytes to directly, plus the storage key finalize confirms.
func (h *AudioAssetsHandler) Presign(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "audio ingestion is not configured")
	}
	var req presignAudioRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(req.Filename))
	mime, ok := audioUploadMIMEs[ext]
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "unsupported audio type — accepted: mp3, wav, m4a, aac, ogg, opus, flac, webm, aiff")
	}

	session := c.Locals("session").(*models.Session)
	id, err := models.NewID()
	if err != nil {
		return err
	}
	fileID, err := models.NewID()
	if err != nil {
		return err
	}

	storageKey := fmt.Sprintf("assets/%s/original%s", id, ext)
	fullKey := storage.TenantKey(reqCtx(c), storageKey)
	uploadURL, err := h.storage.PresignedPutURL(reqCtx(c), fullKey, mime, audioUploadPresignTTL)
	if err != nil {
		return err
	}

	audioType := models.AssetTypeAudio
	title := strings.TrimSuffix(filepath.Base(req.Filename), filepath.Ext(req.Filename))
	asset := &models.Asset{
		ID:        id,
		Title:     title,
		Content:   "", // filled with the transcript's chunks on completion
		Status:    models.AssetStatusPending,
		Type:      &audioType,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}
	// A size-0 file row records the pending original so the object key is known
	// at finalize (real size is stamped there via Head). Written with the asset
	// atomically; an abandoned upload leaves only a harmless pending asset.
	file := &models.AssetFile{
		ID:           fileID,
		AssetID:      id,
		OriginalName: filepath.Base(req.Filename),
		MimeType:     mime,
		SizeBytes:    0,
		S3Key:        fullKey,
	}
	if err := h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(asset).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(file).Exec(ctx)
		return err
	}); err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(presignAudioResponse{
		Asset:      asset,
		UploadURL:  uploadURL,
		StorageKey: storageKey,
		Method:     fiber.MethodPut,
		Headers:    map[string]string{fiber.HeaderContentType: mime},
	})
}

type finalizeAudioRequest struct {
	AssetID string `json:"asset_id" validate:"required"`
	// PinnedModel optionally overrides TRANSCRIBE_MODEL for this run.
	PinnedModel string `json:"model"`
}

// Finalize confirms the uploaded object (Head → size + cap), then enqueues the
// first transcription run atomically. Idempotent: the run_key is deterministic,
// so a duplicate finalize hits the job's (asset_id, run_key) uniqueness.
func (h *AudioAssetsHandler) Finalize(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "audio ingestion is not configured")
	}
	var req finalizeAudioRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	asset, err := h.repo.GetByID(reqCtx(c), req.AssetID)
	if err != nil || asset.Type == nil || *asset.Type != models.AssetTypeAudio {
		return fiber.NewError(fiber.StatusNotFound, "audio asset not found")
	}
	if err := h.prepareAndEnqueue(c, asset, "run-1", req.PinnedModel); err != nil {
		return err
	}
	return c.JSON(asset)
}

// Extract manually starts the first transcription run when finalize didn't (e.g.
// the service was down at upload time). 409 when a run already exists — use
// retry or reextract instead.
func (h *AudioAssetsHandler) Extract(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "audio ingestion is not configured")
	}
	asset, err := h.loadAudioAsset(c)
	if err != nil {
		return err
	}
	if _, err := h.extractions.GetLatestByAsset(reqCtx(c), asset.ID); err == nil {
		return fiber.NewError(fiber.StatusConflict, "an extraction already exists — use retry or reextract")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var body struct {
		PinnedModel string `json:"model"`
	}
	_ = c.BodyParser(&body)
	if err := h.prepareAndEnqueue(c, asset, "run-1", body.PinnedModel); err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"asset_id": asset.ID, "run_key": "run-1", "status": "enqueued"})
}

// Reextract forces a fresh full run under a new run_key (optionally pinning a
// model). Prior chunks are replaced on completion.
func (h *AudioAssetsHandler) Reextract(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "audio ingestion is not configured")
	}
	asset, err := h.loadAudioAsset(c)
	if err != nil {
		return err
	}
	var body struct {
		PinnedModel string `json:"model"`
	}
	_ = c.BodyParser(&body)
	runID, err := models.NewID()
	if err != nil {
		return err
	}
	runKey := "run-" + runID
	if err := h.prepareAndEnqueue(c, asset, runKey, body.PinnedModel); err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"asset_id": asset.ID, "run_key": runKey, "status": "enqueued"})
}

// Retry re-drives only the failed segments of the latest run (same run_key): it
// flips them back to pending and re-enqueues, so completed segments are not
// reprocessed.
func (h *AudioAssetsHandler) Retry(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "audio ingestion is not configured")
	}
	asset, err := h.loadAudioAsset(c)
	if err != nil {
		return err
	}
	ext, err := h.extractions.GetLatestByAsset(reqCtx(c), asset.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "no extraction to retry")
		}
		return err
	}
	if ext.Status == models.AudioExtractionStatusComplete {
		return fiber.NewError(fiber.StatusConflict, "extraction already complete — nothing to retry")
	}
	file, err := h.fileRepo.GetByAssetID(reqCtx(c), asset.ID)
	if err != nil || file == nil {
		return fiber.NewError(fiber.StatusBadRequest, "asset has no uploaded audio")
	}
	// CR8: re-validate the object size even on retry, before re-enqueueing.
	size, ferr := h.headWithinCap(reqCtx(c), file.S3Key)
	if ferr != nil {
		return ferr
	}

	// The segment reset, extraction status flip, and the River enqueue all commit
	// together (CR9): if the enqueue fails, the reset/update roll back, so the
	// extraction never lands in `transcribing` with no job behind it. A zero-reset
	// aborts the tx and maps to 409.
	session := c.Locals("session").(*models.Session)
	now := time.Now().UTC()
	err = h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*models.AudioSegment)(nil)).
			Set("status = ?", models.AudioSegmentStatusPending).
			Set("failure_reason = ''").
			Set("updated_at = ?", now).
			Where("extraction_id = ?", ext.ID).
			Where("status = ?", models.AudioSegmentStatusFailed).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNoFailedSegments
		}
		if _, err := tx.NewUpdate().Model((*models.AudioExtraction)(nil)).
			Set("status = ?", models.AudioExtractionStatusTranscribing).
			Set("failure_reason = ''").
			Set("updated_at = ?", now).
			Where("id = ?", ext.ID).
			Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*models.AssetFile)(nil)).
			Set("size_bytes = ?", size).
			Set("updated_at = ?", now).
			Where("asset_id = ?", asset.ID).
			Exec(ctx); err != nil {
			return err
		}
		return h.audioJobs.EnqueueProcessAudioTx(ctx, tx.Tx, asset.ID, session.TenantID, file.OriginalName, file.MimeType, relativeAudioKey(asset.ID, file.OriginalName), ext.RunKey, ext.TranscribeModel)
	})
	if errors.Is(err, errNoFailedSegments) {
		return fiber.NewError(fiber.StatusConflict, "no failed segments to retry")
	}
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"asset_id": asset.ID, "run_key": ext.RunKey, "status": "enqueued"})
}

type audioStatusResponse struct {
	Extraction *models.AudioExtraction `json:"extraction"`
	Segments   []models.AudioSegment   `json:"segments"`
}

// Status returns the latest extraction with per-segment progress.
func (h *AudioAssetsHandler) Status(c *fiber.Ctx) error {
	asset, err := h.loadAudioAsset(c)
	if err != nil {
		return err
	}
	ext, err := h.extractions.GetLatestByAsset(reqCtx(c), asset.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "no extraction for this asset")
		}
		return err
	}
	segs, err := h.segments.ListByExtraction(reqCtx(c), ext.ID)
	if err != nil {
		return err
	}
	return c.JSON(audioStatusResponse{Extraction: ext, Segments: segs})
}

type transcriptEntry struct {
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Label      string  `json:"label"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Language   string  `json:"language"`
	IsSpeech   bool    `json:"is_speech"`
}

// Transcript returns the latest extraction's raw transcript in timeline order,
// each span carrying its original-timeline offsets and a "12:03" label. Scoped
// to the latest run so a re-extraction doesn't concatenate an older run's spans.
func (h *AudioAssetsHandler) Transcript(c *fiber.Ctx) error {
	asset, err := h.loadAudioAsset(c)
	if err != nil {
		return err
	}
	ext, err := h.extractions.GetLatestByAsset(reqCtx(c), asset.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Asset exists but hasn't been transcribed yet — empty transcript.
			return c.JSON(fiber.Map{"asset_id": asset.ID, "transcript": []transcriptEntry{}})
		}
		return err
	}
	utts, err := h.utterances.ListByExtraction(reqCtx(c), ext.ID)
	if err != nil {
		return err
	}
	entries := make([]transcriptEntry, 0, len(utts))
	for i := range utts {
		u := &utts[i]
		entries = append(entries, transcriptEntry{
			StartMs:    u.StartMs,
			EndMs:      u.EndMs,
			Label:      formatAudioTimecode(u.StartMs),
			Text:       u.Text,
			Confidence: u.Confidence,
			Language:   u.Language,
			IsSpeech:   u.IsSpeech,
		})
	}
	return c.JSON(fiber.Map{"asset_id": asset.ID, "transcript": entries})
}

// prepareAndEnqueue is the shared trigger path for finalize/extract/reextract:
// it loads the asset's file row, enforces the upload-size cap via Head (CR8 —
// so an oversized object can never be enqueued by skipping finalize), persists
// the confirmed size, and enqueues the run — the size update and enqueue commit
// together. Callers shape their own response on success.
func (h *AudioAssetsHandler) prepareAndEnqueue(c *fiber.Ctx, asset *models.Asset, runKey, pinnedModel string) error {
	file, err := h.fileRepo.GetByAssetID(reqCtx(c), asset.ID)
	if err != nil || file == nil {
		return fiber.NewError(fiber.StatusBadRequest, "no pending upload for this asset — call presign first")
	}
	size, ferr := h.headWithinCap(reqCtx(c), file.S3Key)
	if ferr != nil {
		return ferr
	}
	file.SizeBytes = size
	file.UpdatedAt = time.Now().UTC()
	session := c.Locals("session").(*models.Session)
	storageKey := relativeAudioKey(asset.ID, file.OriginalName)
	return h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model(file).Column("size_bytes", "updated_at").WherePK().Exec(ctx); err != nil {
			return err
		}
		return h.audioJobs.EnqueueProcessAudioTx(ctx, tx.Tx, asset.ID, session.TenantID, file.OriginalName, file.MimeType, storageKey, runKey, pinnedModel)
	})
}

// headWithinCap confirms the uploaded object exists and is within the size cap,
// returning its size or a caller-facing 400/413. Enforced before every enqueue
// so the worker never probes an oversized object (CWE-400).
func (h *AudioAssetsHandler) headWithinCap(ctx context.Context, s3Key string) (int64, error) {
	info, err := h.storage.Head(ctx, s3Key)
	if err != nil || info == nil || info.Size == 0 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "upload not found — PUT the file to upload_url before processing")
	}
	if info.Size > maxAudioUploadBytes {
		return 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("audio exceeds the maximum size of %d GiB", maxAudioUploadBytes>>30))
	}
	return info.Size, nil
}

// loadAudioAsset loads the path :id asset, 404ing when it is missing or not an
// AUDIO asset (cross-tenant is already 404 via the repo scope).
func (h *AudioAssetsHandler) loadAudioAsset(c *fiber.Ctx) (*models.Asset, error) {
	asset, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil || asset.Type == nil || *asset.Type != models.AssetTypeAudio {
		return nil, fiber.NewError(fiber.StatusNotFound, "audio asset not found")
	}
	return asset, nil
}

// relativeAudioKey rebuilds the tenant-relative object path presign wrote, from
// the asset id + the stored original filename's extension.
func relativeAudioKey(assetID, originalName string) string {
	return fmt.Sprintf("assets/%s/original%s", assetID, strings.ToLower(filepath.Ext(originalName)))
}

// formatAudioTimecode renders milliseconds as M:SS (or H:MM:SS past an hour) for
// transcript labels — the same shape process_audio stamps on chunk anchors.
func formatAudioTimecode(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	totalSec := ms / 1000
	hh := totalSec / 3600
	mm := (totalSec % 3600) / 60
	ss := totalSec % 60
	if hh > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hh, mm, ss)
	}
	return fmt.Sprintf("%d:%02d", mm, ss)
}
