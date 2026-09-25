package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
)

// AssetsImageHandler serves the content-bank image extraction surface (CON-281):
// trigger/re-run the vision pipeline, read its status + blocks, and regenerate
// alt text. It is a focused sibling of AssetsHandler (CON-291 split), registered
// on the same /api/content-bank/assets group. A nil imgJobs (no
// IMAGE_SERVICE_ADDR) makes the write endpoints fail fast with 409 — image-service
// is a hard dependency (imageprobe deleted, D6).
type AssetsImageHandler struct {
	repo            repository.AssetRepository
	fileRepo        repository.AssetFileRepository
	extractions     repository.ImageExtractionRepository
	blocks          repository.ImageBlockRepository
	storage         storage.Storage
	db              *bun.DB
	imgJobs         ImageIngestEnqueuer
	image           ImagePreparer // for GenerateAltText
	recorder        *usage.Recorder
	altTextModel    string
	altTextMaxChars int
	auth            fiber.Handler
}

func NewAssetsImageHandler(
	repo repository.AssetRepository,
	fileRepo repository.AssetFileRepository,
	extractions repository.ImageExtractionRepository,
	blocks repository.ImageBlockRepository,
	store storage.Storage,
	db *bun.DB,
	imgJobs ImageIngestEnqueuer,
	preparer ImagePreparer,
	recorder *usage.Recorder,
	altTextModel string,
	altTextMaxChars int,
	auth fiber.Handler,
) *AssetsImageHandler {
	return &AssetsImageHandler{
		repo:            repo,
		fileRepo:        fileRepo,
		extractions:     extractions,
		blocks:          blocks,
		storage:         store,
		db:              db,
		imgJobs:         imgJobs,
		image:           preparer,
		recorder:        recorder,
		altTextModel:    altTextModel,
		altTextMaxChars: altTextMaxChars,
		auth:            auth,
	}
}

func (h *AssetsImageHandler) Register(app *fiber.App) {
	g := app.Group("/api/content-bank/assets")
	g.Post("/:id/image/extract", h.auth, h.Extract)
	g.Get("/:id/image", h.auth, h.Status)
	g.Post("/:id/image/reextract", h.auth, h.Reextract)
	g.Post("/:id/image/alt-text", h.auth, h.RegenerateAltText)
}

// configured reports whether image ingestion is wired (enqueuer + storage + db).
func (h *AssetsImageHandler) configured() bool {
	return h.imgJobs != nil && h.storage != nil && h.db != nil
}

// extractionEnqueuedResponse is the 202 body of the audio/image extract, retry
// and reextract triggers.
type extractionEnqueuedResponse struct {
	AssetID string `json:"asset_id"`
	RunKey  string `json:"run_key"`
	Status  string `json:"status" example:"enqueued"`
}

// pinnedModelRequest optionally pins the model for one run.
type pinnedModelRequest struct {
	Model string `json:"model"`
}

// Extract godoc
// @Summary      Start image extraction
// @Description  Starts the first vision run for an IMG asset when the upload-time enqueue didn't (e.g. image-service was down). 409 when a run already exists — use reextract. Optional body pins the extraction model.
// @Tags         content-bank
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string              true   "Asset Sqid"
// @Param        body  body      pinnedModelRequest  false  "Optional model pin"
// @Success      202   {object}  extractionEnqueuedResponse
// @Failure      400   {object}  map[string]string  "asset has no uploaded image"
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string  "not configured, or a run already exists"
// @Router       /api/content-bank/assets/{id}/image/extract [post]
func (h *AssetsImageHandler) Extract(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "image processing is not configured")
	}
	asset, err := h.loadImageAsset(c)
	if err != nil {
		return err
	}
	if _, err := h.extractions.GetLatestByAsset(reqCtx(c), asset.ID); err == nil {
		return fiber.NewError(fiber.StatusConflict, "an extraction already exists — use reextract")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	model := pinnedModelFromBody(c)
	if err := h.enqueue(c, asset, "run-1", model); err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(extractionEnqueuedResponse{AssetID: asset.ID, RunKey: "run-1", Status: "enqueued"})
}

// Reextract godoc
// @Summary      Re-extract an image
// @Description  Forces a fresh full vision run under a new run_key (optionally pinning a model). Prior blocks/chunks are replaced on completion; prior extraction rows are kept (additive history). A user-edited alt text is never overwritten.
// @Tags         content-bank
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string              true   "Asset Sqid"
// @Param        body  body      pinnedModelRequest  false  "Optional model pin"
// @Success      202   {object}  extractionEnqueuedResponse
// @Failure      400   {object}  map[string]string  "asset has no uploaded image"
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string  "image processing is not configured"
// @Router       /api/content-bank/assets/{id}/image/reextract [post]
func (h *AssetsImageHandler) Reextract(c *fiber.Ctx) error {
	if !h.configured() {
		return fiber.NewError(fiber.StatusConflict, "image processing is not configured")
	}
	asset, err := h.loadImageAsset(c)
	if err != nil {
		return err
	}
	runID, err := models.NewID()
	if err != nil {
		return err
	}
	runKey := "run-" + runID
	model := pinnedModelFromBody(c)
	if err := h.enqueue(c, asset, runKey, model); err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(extractionEnqueuedResponse{AssetID: asset.ID, RunKey: runKey, Status: "enqueued"})
}

type imageStatusResponse struct {
	Extraction *models.ImageExtraction `json:"extraction"`
	Blocks     []models.ImageBlock     `json:"blocks"`
}

// Status godoc
// @Summary      Get image extraction
// @Description  Returns the latest vision run for an IMG asset (status, shape, quality flags, `failure_code`/`failure_reason`, cost) with its structured region blocks. Each block carries a normalized `bbox` anchor on the source image.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Asset Sqid"
// @Success      200  {object}  imageStatusResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string  "not an IMG asset, or no extraction yet"
// @Router       /api/content-bank/assets/{id}/image [get]
func (h *AssetsImageHandler) Status(c *fiber.Ctx) error {
	asset, err := h.loadImageAsset(c)
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
	blocks, err := h.blocks.ListByExtraction(reqCtx(c), ext.ID)
	if err != nil {
		return err
	}
	return c.JSON(imageStatusResponse{Extraction: ext, Blocks: blocks})
}

// altTextResponse is the regenerated alt text.
type altTextResponse struct {
	AssetID string `json:"asset_id"`
	AltText string `json:"alt_text"`
}

// RegenerateAltText godoc
// @Summary      Regenerate image alt text
// @Description  Synchronously re-runs only alt-text generation for the stored image and overwrites the asset's alt text — an explicit user action, so it replaces even a user-edited value and does not flip `alt_text_edited_by_user`.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Asset Sqid"
// @Success      200  {object}  altTextResponse
// @Failure      400  {object}  map[string]string  "no uploaded image, or it could not be read"
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string  "image processing is not configured"
// @Failure      503  {object}  map[string]string  "image-service temporarily unavailable"
// @Router       /api/content-bank/assets/{id}/image/alt-text [post]
func (h *AssetsImageHandler) RegenerateAltText(c *fiber.Ctx) error {
	if h.image == nil || h.storage == nil {
		return fiber.NewError(fiber.StatusConflict, "image processing is not configured")
	}
	asset, err := h.loadImageAsset(c)
	if err != nil {
		return err
	}
	file, err := h.fileRepo.GetByAssetID(reqCtx(c), asset.ID)
	if err != nil || file == nil {
		return fiber.NewError(fiber.StatusBadRequest, "asset has no uploaded image")
	}
	getURL, err := h.storage.PresignedGetURL(reqCtx(c), file.S3Key, PresignedURLTTL)
	if err != nil {
		return err
	}
	res, err := h.image.GenerateAltText(reqCtx(c), imageclient.GenerateAltTextOptions{
		SourceURL: getURL,
		MaxChars:  h.altTextMaxChars,
		Model:     h.altTextModel,
	})
	if err != nil {
		if imageclient.IsInvalidImage(err) || imageclient.IsUnsupportedImage(err) {
			return fiber.NewError(fiber.StatusBadRequest, "the image could not be read")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "image processing is temporarily unavailable; please retry")
	}
	for _, u := range res.Usage {
		h.recorder.RecordResp(reqCtx(c), llm.VendorGemini, u.Model, "alt_text", llm.VisionUsage{Step: u.Step, InputTokens: u.Input, OutputTokens: u.Output})
	}
	alt := strings.TrimSpace(res.AltText)
	if utf8.RuneCountInString(alt) > maxAltTextLen() {
		alt = string([]rune(alt)[:maxAltTextLen()])
	}
	if err := h.repo.SetAltText(reqCtx(c), asset.ID, alt); err != nil {
		return err
	}
	return c.JSON(altTextResponse{AssetID: asset.ID, AltText: alt})
}

// enqueue confirms the asset has an uploaded original and enqueues a run in a
// transaction (so a failed enqueue leaves no dangling state).
func (h *AssetsImageHandler) enqueue(c *fiber.Ctx, asset *models.Asset, runKey, pinnedModel string) error {
	file, err := h.fileRepo.GetByAssetID(reqCtx(c), asset.ID)
	if err != nil || file == nil {
		return fiber.NewError(fiber.StatusBadRequest, "asset has no uploaded image")
	}
	session := c.Locals("session").(*models.Session)
	storageKey := relativeImageKey(asset.ID, file.OriginalName)
	return h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		return h.imgJobs.EnqueueProcessImageTx(ctx, tx.Tx, asset.ID, session.TenantID, file.OriginalName, file.MimeType, storageKey, runKey, pinnedModel)
	})
}

// loadImageAsset loads the path :id asset, 404ing when it is missing or not an
// IMG asset (cross-tenant is already 404 via the repo scope).
func (h *AssetsImageHandler) loadImageAsset(c *fiber.Ctx) (*models.Asset, error) {
	asset, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil || asset.Type == nil || *asset.Type != models.AssetTypeImage {
		return nil, fiber.NewError(fiber.StatusNotFound, "image asset not found")
	}
	return asset, nil
}

// pinnedModelFromBody reads an optional {"model": "..."} extraction-model
// override from the request body; absent/invalid bodies mean "use the default".
func pinnedModelFromBody(c *fiber.Ctx) string {
	var body struct {
		Model string `json:"model"`
	}
	_ = c.BodyParser(&body)
	return strings.TrimSpace(body.Model)
}

// relativeImageKey rebuilds the tenant-relative object path the upload wrote,
// from the asset id + the stored original filename's extension.
func relativeImageKey(assetID, originalName string) string {
	return fmt.Sprintf("assets/%s/original%s", assetID, strings.ToLower(filepath.Ext(originalName)))
}
