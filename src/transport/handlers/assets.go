package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/netguard"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/ingest"
)

const (
	maxMarkdownUploadSize = 10 << 20 // 10 MB
	maxPDFUploadSize      = 50 << 20 // 50 MB
	// maxDocumentUploadSize caps office/text document uploads. Larger
	// than markdown because a real .pptx/.xlsx carries embedded media we discard
	// but still receive.
	maxDocumentUploadSize = 50 << 20 // 50 MB
)

// imageUploadMIMEs is the CON-281 accept list for content-bank image uploads:
// the raster formats image-service decodes (libvips), mapped to the MIME stored
// in asset_files.mime_type and bound on the stored original. It replaces the old
// imageprobe allowlist and adds heic/heif/avif/tiff/bmp. The extension is
// advisory routing only — image-service sniffs the body's magic bytes
// authoritatively. SVG/vector is deliberately absent (terminal reject).
var imageUploadMIMEs = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
	".heic": "image/heic",
	".heif": "image/heif",
	".avif": "image/avif",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".bmp":  "image/bmp",
}

// documentUploadMIMEs is the CON-280 accept list: the office/text document
// extensions document-service parses, mapped to the MIME stored in
// asset_files.mime_type and used as the upload content-type. Membership also
// drives detectUploadKind. The extension is advisory routing only —
// document-service sniffs the body's magic bytes authoritatively. `.md`/`.pdf`
// are deliberately absent: they keep their own ingestion paths.
var documentUploadMIMEs = map[string]string{
	".docx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".docm":  "application/vnd.ms-word.document.macroenabled.12",
	".dotx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.template",
	".xlsx":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xlsm":  "application/vnd.ms-excel.sheet.macroenabled.12",
	".xltx":  "application/vnd.openxmlformats-officedocument.spreadsheetml.template",
	".pptx":  "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".pptm":  "application/vnd.ms-powerpoint.presentation.macroenabled.12",
	".potx":  "application/vnd.openxmlformats-officedocument.presentationml.template",
	".odt":   "application/vnd.oasis.opendocument.text",
	".ods":   "application/vnd.oasis.opendocument.spreadsheet",
	".odp":   "application/vnd.oasis.opendocument.presentation",
	".fodt":  "application/vnd.oasis.opendocument.text",
	".fods":  "application/vnd.oasis.opendocument.spreadsheet",
	".fodp":  "application/vnd.oasis.opendocument.presentation",
	".epub":  "application/epub+zip",
	".csv":   "text/csv",
	".tsv":   "text/tab-separated-values",
	".html":  "text/html",
	".xhtml": "application/xhtml+xml",
	".eml":   "message/rfc822",
	".rtf":   "application/rtf",
	".txt":   "text/plain",
	".log":   "text/plain",
}

// PDFIngestEnqueuer enqueues a PDF-ingestion job in the caller's transaction.
// Implemented by *queues.Enqueuer; a narrow interface here keeps the
// handler off the jobs package.
type PDFIngestEnqueuer interface {
	EnqueueProcessPDFTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType string) error
}

// URLIngestEnqueuer enqueues a URL-scrape job in the caller's transaction.
// Implemented by *queues.Enqueuer.
type URLIngestEnqueuer interface {
	EnqueueProcessURLTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, sourceURL string, refresh bool) error
}

// DocumentIngestEnqueuer enqueues a document-ingestion job in the caller's
// transaction. Implemented by *queues.Enqueuer; a narrow interface
// here keeps the handler off the jobs package.
type DocumentIngestEnqueuer interface {
	EnqueueProcessDocumentTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey string) error
}

// ImageIngestEnqueuer enqueues an image-ingestion job in the caller's
// transaction. Implemented by *queues.Enqueuer; a narrow interface
// here keeps the handler off the jobs package. Nil (no IMAGE_SERVICE_ADDR) makes
// image uploads fail fast — image-service is a hard dependency (D6).
type ImageIngestEnqueuer interface {
	EnqueueProcessImageTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error
}

// ImageReembedEnqueuer enqueues a re-embed of an image asset's edited
// description + its stored region blocks, without re-running vision.
// Implemented by *queues.Enqueuer.
type ImageReembedEnqueuer interface {
	EnqueueReembedImage(ctx context.Context, assetID, tenantID string) error
}

// URLScrapeGate reports whether URL scraping is currently configured, so the
// endpoint can fail fast with 409. Implemented by *firecrawl.Client.
type URLScrapeGate interface {
	HasKey(ctx context.Context) bool
}

type AssetsHandler struct {
	repo      repository.AssetRepository
	fileRepo  repository.AssetFileRepository
	imageRepo repository.AssetImageRepository
	storage   storage.Storage
	db        *bun.DB
	auth      fiber.Handler
	limiter   *entitlements.Limiter // CON-295 entitlement quota gate (nil-safe)

	// onSave triggers async embedding for text-based Asset saves (JSON create/update + MD upload).
	onSave func(assetID, title, content, tenantID string)
	// pdfJobs enqueues PDF ingestion. Nil disables it (the asset is
	// created but left pending).
	pdfJobs PDFIngestEnqueuer
	// urlJobs enqueues URL scraping; scrapeGate reports key presence.
	// Nil urlJobs / scrapeGate makes the URL endpoint return 409.
	urlJobs    URLIngestEnqueuer
	scrapeGate URLScrapeGate
	// docJobs enqueues document ingestion. Nil makes document uploads
	// fail fast with a "not configured" message.
	docJobs DocumentIngestEnqueuer
	// imgJobs enqueues image ingestion. Nil makes image uploads fail
	// fast — image-service is a hard dependency (imageprobe was deleted, D6).
	imgJobs ImageIngestEnqueuer
	// imgReembed re-embeds an image asset after its description is edited.
	// Nil skips the re-embed (image ingestion not configured).
	imgReembed ImageReembedEnqueuer
	// chunks backs GET /:id/chunks. Nil answers 409.
	chunks AssetChunkLister
}

// AssetsOptions carries the handler's nil-safe collaborators.
type AssetsOptions struct {
	Limiter *entitlements.Limiter
	// Chunks backs GET /:id/chunks; nil answers 409.
	Chunks AssetChunkLister
	// ImageReembed re-embeds an image asset after its description is edited;
	// nil skips it (image ingestion not configured).
	ImageReembed ImageReembedEnqueuer
}

func NewAssetsHandler(
	repo repository.AssetRepository,
	fileRepo repository.AssetFileRepository,
	imageRepo repository.AssetImageRepository,
	store storage.Storage,
	db *bun.DB,
	pdfJobs PDFIngestEnqueuer,
	urlJobs URLIngestEnqueuer,
	scrapeGate URLScrapeGate,
	docJobs DocumentIngestEnqueuer,
	imgJobs ImageIngestEnqueuer,
	auth fiber.Handler,
	onSave func(assetID, title, content, tenantID string),
	opts AssetsOptions,
) *AssetsHandler {
	return &AssetsHandler{
		limiter:    opts.Limiter,
		chunks:     opts.Chunks,
		imgReembed: opts.ImageReembed,
		repo:       repo,
		fileRepo:   fileRepo,
		imageRepo:  imageRepo,
		storage:    store,
		db:         db,
		auth:       auth,
		onSave:     onSave,
		pdfJobs:    pdfJobs,
		urlJobs:    urlJobs,
		scrapeGate: scrapeGate,
		docJobs:    docJobs,
		imgJobs:    imgJobs,
	}
}

func (h *AssetsHandler) Register(app *fiber.App) {
	g := app.Group("/api/content-bank/assets")
	g.Get("/", h.auth, h.List)
	g.Post("/", h.auth, h.Create)
	g.Post("/upload", h.auth, h.Upload)
	g.Post("/url", h.auth, h.CreateURL)
	g.Post("/tags", h.auth, h.BulkTag)
	g.Get("/:id", h.auth, h.Get)
	g.Get("/:id/chunks", h.auth, h.Chunks)
	g.Put("/:id", h.auth, h.Update)
	g.Delete("/:id", h.auth, h.Delete)
}

type createAssetRequest struct {
	Title   string             `json:"title"   validate:"required"`
	Content string             `json:"content" validate:"required"`
	AltText string             `json:"alt_text"`
	TagIDs  models.StringSlice `json:"tag_ids"`
}

// updateAssetRequest carries a whole-resource write. Content is not
// `validate:"required"` because an image asset's description may legitimately be
// empty; Update enforces it for the document types that still need
// it once the asset's type is known.
//
// AltText and TagIDs are pointers so the handler can tell an omitted field from
// one sent empty: a nil pointer leaves the stored value alone, a present one
// (including "" or []) replaces it. Before CON-279 both were assigned
// unconditionally, so a title/content-only PUT — all the document editor sends —
// silently wiped an asset's tags and alt text.
type updateAssetRequest struct {
	Title   string              `json:"title"   validate:"required"`
	Content string              `json:"content"`
	AltText *string             `json:"alt_text"`
	TagIDs  *models.StringSlice `json:"tag_ids"`
}

// List godoc
// @Summary      List assets
// @Description  Returns all content bank assets ordered by creation date.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   models.Asset
// @Failure      401  {object}  map[string]string
// @Router       /api/content-bank/assets [get]
func (h *AssetsHandler) List(c *fiber.Ctx) error {
	assets, err := h.repo.List(reqCtx(c))
	if err != nil {
		return err
	}
	for i := range assets {
		h.decorateFile(&assets[i])
	}
	h.decorateImagesBatch(reqCtx(c), assets)
	return c.JSON(assets)
}

// decorateImagesBatch hydrates mirrored images for a list of assets in one query,
// URL-decorating each. No-op when the image repo is unwired.
func (h *AssetsHandler) decorateImagesBatch(ctx context.Context, assets []models.Asset) {
	if h.imageRepo == nil || len(assets) == 0 {
		return
	}
	ids := make([]string, len(assets))
	for i := range assets {
		ids[i] = assets[i].ID
	}
	byAsset, err := h.imageRepo.ListByAssetIDs(ctx, ids)
	if err != nil {
		return
	}
	for i := range assets {
		imgs, ok := byAsset[assets[i].ID]
		if !ok {
			continue
		}
		for j := range imgs {
			if h.storage != nil && imgs[j].S3Key != "" {
				u := h.storage.PublicURL(imgs[j].S3Key)
				imgs[j].URL = &u
			}
		}
		assets[i].Images = imgs
	}
}

// decorateFile fills File.URL (the original), File.ThumbnailURL and
// File.NormalizedURL from their s3 keys using the public storage URL, when
// present. URL is the original bytes an image viewer downloads;
// NormalizedURL is the browser-drawable derivative — what the asset screen shows
// for HEIC/TIFF, which no browser decodes; ThumbnailURL is the
// PDF/first-page preview and the image grid thumbnail.
func (h *AssetsHandler) decorateFile(asset *models.Asset) {
	if asset == nil || asset.File == nil || h.storage == nil {
		return
	}
	if asset.File.S3Key != "" {
		u := h.storage.PublicURL(asset.File.S3Key)
		asset.File.URL = &u
	}
	if asset.File.ThumbnailS3Key != nil && *asset.File.ThumbnailS3Key != "" {
		u := h.storage.PublicURL(*asset.File.ThumbnailS3Key)
		asset.File.ThumbnailURL = &u
	}
	if asset.File.NormalizedS3Key != nil && *asset.File.NormalizedS3Key != "" {
		u := h.storage.PublicURL(*asset.File.NormalizedS3Key)
		asset.File.NormalizedURL = &u
	}
}

// decorateImages hydrates a URL asset's mirrored images and fills each
// image's public URL from its s3_key. No-op when the image repo/storage are
// unwired or the asset has no images.
func (h *AssetsHandler) decorateImages(ctx context.Context, asset *models.Asset) {
	if asset == nil || h.imageRepo == nil {
		return
	}
	images, err := h.imageRepo.GetByAssetID(ctx, asset.ID)
	if err != nil || len(images) == 0 {
		return
	}
	for i := range images {
		if h.storage != nil && images[i].S3Key != "" {
			u := h.storage.PublicURL(images[i].S3Key)
			images[i].URL = &u
		}
	}
	asset.Images = images
}

// Create godoc
// @Summary      Create asset
// @Description  Creates a new content bank asset. The created_by field is set from the authenticated session.
// @Tags         content-bank
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      createAssetRequest  true  "Asset payload"
// @Success      201   {object}  models.Asset
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/content-bank/assets [post]
func (h *AssetsHandler) Create(c *fiber.Ctx) error {
	var req createAssetRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	// The content_bank_assets quota gates a new asset.
	assetQuota, err := requireQuota(c, h.limiter, "content_bank_assets")
	if err != nil {
		return err
	}

	altText, err := normalizeAltText(req.AltText)
	if err != nil {
		return err
	}

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}

	asset := &models.Asset{
		ID:        id,
		Title:     req.Title,
		Content:   req.Content,
		AltText:   altText,
		Status:    models.AssetStatusPending,
		TagIDs:    nullSlice(req.TagIDs),
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}
	if err := h.repo.Create(reqCtx(c), asset); err != nil {
		return err
	}
	// The asset now exists — fire any near-limit crossing.
	assetQuota.dispatch(reqCtx(c))

	if h.onSave != nil {
		tenantID, _ := tenantctx.From(reqCtx(c))
		backgroundTasks.Go("assets.on_save", func() { h.onSave(asset.ID, asset.Title, asset.Content, tenantID) })
	}

	return c.Status(fiber.StatusCreated).JSON(asset)
}

// uploadResult is the per-file outcome in a batch upload.
type uploadResult struct {
	Filename string `json:"filename"`
	AssetID  string `json:"asset_id,omitempty"`
	Status   string `json:"status"` // "created" | "failed"
	Error    string `json:"error,omitempty"`
	// Code is a stable, machine-readable companion to Error on a failed result:
	// the client matches the code and falls back to the prose when it
	// is unknown. Empty on a created result. See models.UploadCode*.
	Code  string        `json:"code,omitempty"`
	Asset *models.Asset `json:"asset,omitempty"`
	// deduplicated marks a created result that is the tenant's existing asset
	// with the same bytes rather than a new one.
	deduplicated bool
}

// fail stamps a terminal per-file outcome with a machine-readable code and its
// human-readable message, keeping the two in lockstep at every
// rejection site.
func (r *uploadResult) fail(code, msg string) uploadResult {
	r.Status = "failed"
	r.Code = code
	r.Error = msg
	return *r
}

// uploadResponse is the batch upload outcome: one result per file, in order.
type uploadResponse struct {
	Results []uploadResult `json:"results"`
}

// Upload godoc
// @Summary      Upload Markdown, PDF, image, or document file(s)
// @Description  Accepts one or more files under the field `files`, routed by extension. Files are processed independently — one failure does not block others; each gets a result with `status` `created` or `failed`.
// @Description  - `.md` (max 10 MB): converted to BlockNote JSON synchronously (`MD`, `ready`).
// @Description  - `.pdf` (max 50 MB): stored, then processed asynchronously (text extraction, page-aware chunking, embedding, thumbnail) as `PDF` (`pending` → `ready`/`partial`/`failed`).
// @Description  - Images — JPEG, PNG, WebP, GIF, HEIC/HEIF, AVIF, TIFF, BMP (operator-set cap, default 50 MB; SVG is rejected): stored and ingested asynchronously by image-service (normalize, EXIF-strip, vision description, region extraction, alt text) as `IMG` (`pending` → `ready`/`partial`/`failed`); deduplicated within the tenant by checksum. Poll `GET /{id}/image` for the run.
// @Description  - Office/text documents — .docx/.docm/.dotx, .xlsx/.xlsm/.xltx, .pptx/.pptm/.potx, .odt/.ods/.odp (+ flat), .epub, .csv/.tsv, .html/.xhtml, .eml, .rtf, .txt/.log (max 50 MB; legacy/password-protected OLE2 rejected): parsed asynchronously by document-service into source-anchored chunks as `DOC` (`pending` → `ready`/`partial`/`failed`, with `failure_code`/`failure_reason` on the asset when failed).
// @Description  Audio is not accepted here — use `POST /audio/presign` + `/audio/finalize`.
// @Description  A failed result carries a stable `code` beside the prose `error`: `extension_not_allowed`, `unsupported_media_type`, `vector_rejected`, `too_large`, `empty_file`, `invalid_file`, `dimensions_exceeded`, `quota_exceeded` (content-bank asset or media-storage tier limit), `service_unavailable`, `internal_error`. Match on the code; fall back to the prose for unknown codes.
// @Tags         content-bank
// @Accept       multipart/form-data
// @Produce      json
// @Security     CookieAuth
// @Param        files  formData  file  true  "Markdown, PDF, image, or document file(s)"
// @Success      201    {object}  uploadResponse
// @Failure      400    {object}  map[string]string
// @Failure      401    {object}  map[string]string
// @Router       /api/content-bank/assets/upload [post]
func (h *AssetsHandler) Upload(c *fiber.Ctx) error {
	form, err := c.MultipartForm()
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "expected multipart/form-data")
	}
	files := form.File["files"]
	if len(files) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "no files provided (use field 'files')")
	}

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	results := make([]uploadResult, 0, len(files))
	for _, fh := range files {
		res := uploadResult{Filename: fh.Filename}

		// The processors below Create the asset synchronously, so the next
		// iteration's count reflects the ones already stored.
		quota, qErr := h.requireUploadQuota(c, fh.Size)
		if qErr != nil {
			results = append(results, res.fail(models.UploadCodeQuotaExceeded, qErr.Error()))
			continue
		}

		switch detectUploadKind(fh.Filename) {
		case uploadKindMarkdown:
			res = h.processMarkdownUpload(c, fh, session)
		case uploadKindPDF:
			res = h.processPDFUpload(c, fh, session)
		case uploadKindImage:
			res = h.processImageUpload(c, fh, session)
		case uploadKindDocument:
			res = h.processDocumentUpload(c, fh, session)
		default:
			res = res.fail(models.UploadCodeExtensionNotAllowed, "only .md, .pdf, image, and office/text document files are accepted")
		}
		// Only a stored asset counts — fire the crossing once we know it landed.
		if res.Status == "created" {
			quota.dispatch(reqCtx(c))
		}
		results = append(results, res)
	}

	return c.Status(fiber.StatusCreated).JSON(uploadResponse{Results: results})
}

// uploadQuota is the pair of quota grants one content-bank upload consumes.
type uploadQuota struct {
	assets, media quotaHold
}

// dispatch fires both grants' near-limit crossings. Call it only once the
// asset was stored.
func (q uploadQuota) dispatch(ctx context.Context) {
	q.assets.dispatch(ctx)
	q.media.dispatch(ctx)
}

// uploadQuotaError is a refused upload quota. Its message names the limit for
// a batch result; it unwraps to the limiter's rejection for callers that
// answer with the limiter's own 402/403.
type uploadQuotaError struct {
	msg string
	err error
}

func (e *uploadQuotaError) Error() string { return e.msg }
func (e *uploadQuotaError) Unwrap() error { return e.err }

// requireUploadQuota gates one content-bank upload of size bytes: it becomes a
// content_bank_assets row, and its bytes join media_storage_bytes ("all
// uploaded media"). Checked before storage so a denied file leaves no object.
func (h *AssetsHandler) requireUploadQuota(c *fiber.Ctx, size int64) (uploadQuota, error) {
	assets, err := requireQuota(c, h.limiter, "content_bank_assets")
	if err != nil {
		return uploadQuota{}, &uploadQuotaError{msg: "content bank asset limit reached", err: err}
	}
	media, err := requireQuotaAmount(c, h.limiter, "media_storage_bytes", size)
	if err != nil {
		return uploadQuota{}, &uploadQuotaError{msg: "media storage limit reached", err: err}
	}
	return uploadQuota{assets: assets, media: media}, nil
}

type uploadKind int

const (
	uploadKindUnknown uploadKind = iota
	uploadKindMarkdown
	uploadKindPDF
	uploadKindImage
	uploadKindDocument
)

func detectUploadKind(filename string) uploadKind {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".md":
		return uploadKindMarkdown
	case ".pdf":
		return uploadKindPDF
	case ".svg":
		// Route SVG to the image branch so processImageUpload emits the specific
		// "vector not supported" reject rather than a generic message.
		return uploadKindImage
	}
	// Raster images — advisory extension routing only; image-service
	// sniffs the body's magic bytes authoritatively.
	if _, ok := imageUploadMIMEs[ext]; ok {
		return uploadKindImage
	}
	// Office/text documents — advisory extension routing only;
	// document-service sniffs the body authoritatively.
	if _, ok := documentUploadMIMEs[ext]; ok {
		return uploadKindDocument
	}
	return uploadKindUnknown
}

// created stamps a successful per-file outcome.
func (r *uploadResult) created(a *models.Asset) uploadResult {
	r.AssetID = a.ID
	r.Status = "created"
	r.Asset = a
	return *r
}

// ingester builds the asset-creation use case over the handler's repositories.
func (h *AssetsHandler) ingester() *ingest.Service {
	return &ingest.Service{DB: h.db, Assets: h.repo, Files: h.fileRepo, Images: h.imageRepo}
}

// newUploadAsset builds a pending asset of typ titled after the uploaded
// file's base name.
func newUploadAsset(filename, typ, content, createdBy string) (*models.Asset, error) {
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	return &models.Asset{
		ID:        id,
		Title:     strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename)),
		Content:   content,
		Status:    models.AssetStatusPending,
		Type:      &typ,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: createdBy,
	}, nil
}

func (h *AssetsHandler) processMarkdownUpload(c *fiber.Ctx, fh *multipart.FileHeader, session *models.Session) uploadResult {
	res := uploadResult{Filename: fh.Filename}

	if fh.Size > maxMarkdownUploadSize {
		return res.fail(models.UploadCodeTooLarge, fmt.Sprintf("file exceeds maximum size of %d MB", maxMarkdownUploadSize>>20))
	}

	raw, err := readFormFile(fh, maxMarkdownUploadSize)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not read file")
	}

	asset, err := newUploadAsset(fh.Filename, models.AssetTypeMarkdown, string(raw), session.UserID)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	if err := h.repo.Create(reqCtx(c), asset); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}

	if h.onSave != nil {
		tid, _ := tenantctx.From(reqCtx(c))
		backgroundTasks.Go("assets.on_save", func() { h.onSave(asset.ID, asset.Title, asset.Content, tid) })
	}
	return res.created(asset)
}

func (h *AssetsHandler) processPDFUpload(c *fiber.Ctx, fh *multipart.FileHeader, session *models.Session) uploadResult {
	res := uploadResult{Filename: fh.Filename}

	if fh.Size > maxPDFUploadSize {
		return res.fail(models.UploadCodeTooLarge, fmt.Sprintf("file exceeds maximum size of %d MB", maxPDFUploadSize>>20))
	}

	raw, err := readFormFile(fh, maxPDFUploadSize)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not read file")
	}
	if len(raw) < 4 || string(raw[:4]) != "%PDF" {
		return res.fail(models.UploadCodeInvalidFile, "file is not a valid PDF")
	}

	asset, err := newUploadAsset(fh.Filename, models.AssetTypePDF, "[]", session.UserID)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}

	ctx := reqCtx(c)

	// PDF ingestion needs object storage — the worker re-reads the PDF
	// from it on each attempt — plus the job enqueuer. Without them, create the
	// asset but skip processing (it stays pending).
	if h.storage == nil || h.pdfJobs == nil || h.db == nil {
		if err := h.repo.Create(ctx, asset); err != nil {
			return res.fail(models.UploadCodeInternalError, "could not create asset")
		}
		slog.WarnContext(ctx, "pdf ingestion disabled; asset left pending", logging.AttrComponent, "assets", "asset_id", asset.ID)
		return res.created(asset)
	}

	// The original is stored before the enqueue so the worker can re-read it
	// on each attempt (the bytes can't ride in the River job args).
	blob, err := ingest.PutBlob(ctx, h.storage, storage.TenantKey(ctx, fmt.Sprintf("assets/%s/original.pdf", asset.ID)), raw, "application/pdf")
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not store pdf")
	}
	if _, err := h.ingester().Create(ctx, ingest.NewAsset{
		Asset: asset,
		Blob:  blob,
		Enqueue: func(ctx context.Context, tx *sql.Tx) error {
			return h.pdfJobs.EnqueueProcessPDFTx(ctx, tx, asset.ID, session.TenantID, fh.Filename, "application/pdf")
		},
	}); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}
	return res.created(asset)
}

// processDocumentUpload ingests an office/text document: store the
// original in object storage, then insert the asset and enqueue the extraction
// job atomically. document-service does the authoritative format detection,
// so the handler only does a light OLE2 reject (the common legacy-.doc /
// encrypted-container case) for fast, clear feedback. Bytes land at
// assets/{id}/original.<ext>.
func (h *AssetsHandler) processDocumentUpload(c *fiber.Ctx, fh *multipart.FileHeader, session *models.Session) uploadResult {
	res := uploadResult{Filename: fh.Filename}

	ext := strings.ToLower(filepath.Ext(fh.Filename))
	mimeType, ok := documentUploadMIMEs[ext]
	if !ok {
		// detectUploadKind already gated this; stay defensive.
		return res.fail(models.UploadCodeUnsupportedMediaType, "unsupported document type")
	}

	if fh.Size > maxDocumentUploadSize {
		return res.fail(models.UploadCodeTooLarge, fmt.Sprintf("file exceeds maximum size of %d MB", maxDocumentUploadSize>>20))
	}

	// Document ingestion needs object storage (the worker re-reads the file on
	// each attempt), the job enqueuer, and the DB. docJobs is nil when
	// DOCUMENTS_SERVICE_ADDR is empty, so a doc upload fails fast — before
	// reading the body — instead of stranding a pending asset.
	if h.storage == nil || h.docJobs == nil || h.db == nil {
		return res.fail(models.UploadCodeServiceUnavailable, "document ingestion is not configured")
	}

	raw, err := readFormFile(fh, maxDocumentUploadSize)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not read file")
	}
	if len(raw) == 0 {
		return res.fail(models.UploadCodeEmptyFile, "file is empty")
	}
	// OLE2 is both the legacy binary container (.doc/.xls/.ppt) and the
	// wrapper for password-protected OOXML — neither is supported.
	if isOLE2(raw) {
		return res.fail(models.UploadCodeUnsupportedMediaType, "legacy binary or password-protected Office files are not supported — save as unprotected .docx/.xlsx/.pptx and re-upload")
	}

	asset, err := newUploadAsset(fh.Filename, models.AssetTypeDocument, "[]", session.UserID)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}

	ctx := reqCtx(c)
	// storageKey is the tenant-relative path the worker resolves via
	// storage.TenantKey.
	storageKey := fmt.Sprintf("assets/%s/original%s", asset.ID, ext)
	blob, err := ingest.PutBlob(ctx, h.storage, storage.TenantKey(ctx, storageKey), raw, mimeType)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not store document")
	}
	if _, err := h.ingester().Create(ctx, ingest.NewAsset{
		Asset: asset,
		Blob:  blob,
		Enqueue: func(ctx context.Context, tx *sql.Tx) error {
			return h.docJobs.EnqueueProcessDocumentTx(ctx, tx, asset.ID, session.TenantID, fh.Filename, mimeType, storageKey)
		},
	}); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}
	return res.created(asset)
}

// isOLE2 reports whether b starts with the OLE2 compound-file magic
// (D0 CF 11 E0 A1 B1 1A E1) used by legacy binary Office files (.doc/.xls/.ppt)
// and by the encrypted-OOXML container — neither is supported.
func isOLE2(b []byte) bool {
	const sig = "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"
	return len(b) >= len(sig) && string(b[:len(sig)]) == sig
}

// processImageUpload ingests a content-bank image asset. image-service is the
// single image authority, so ingestion is async: the handler stores the
// original, creates a pending IMG asset + file row, and enqueues a
// process_image job that normalizes, classifies, extracts, describes,
// alt-texts, and embeds it. There is no pure-Go fallback, so an unwired
// service (imgJobs nil) fails the upload. The SHA-256 of the original bytes is
// computed here so upload-time dedupe works despite the async processing.
// Bytes land at assets/{id}/original.<ext>.
func (h *AssetsHandler) processImageUpload(c *fiber.Ctx, fh *multipart.FileHeader, session *models.Session) uploadResult {
	res := uploadResult{Filename: fh.Filename}

	mimeType, raw, fail := h.readImageUpload(fh)
	if fail != nil {
		return res.fail(fail.code, fail.msg)
	}
	return h.ingestImage(c, session, imageIngest{Filename: fh.Filename, MimeType: mimeType, Raw: raw})
}

// imageIngest is a validated image ready to become an IMG asset: the name it
// was sent under, the MIME stored for it, and its original bytes. Origin,
// OriginRef and a non-empty (user-written) AltText apply only when a new asset
// is created.
type imageIngest struct {
	Filename  string
	MimeType  string
	Raw       []byte
	Origin    string
	OriginRef *models.AssetOriginRef
	AltText   string
}

// ingestImage stores a validated image as a pending IMG asset and enqueues its
// process_image job in the same transaction, or returns the tenant's existing
// asset with the same bytes (marked deduplicated, its provenance untouched).
func (h *AssetsHandler) ingestImage(c *fiber.Ctx, session *models.Session, in imageIngest) uploadResult {
	res := uploadResult{Filename: in.Filename}
	ctx := reqCtx(c)
	svc := h.ingester()
	// Dedupe within the tenant: the same image uploaded twice returns the
	// first asset instead of a near-duplicate.
	checksum := ingest.Checksum(in.Raw)
	if a := svc.FindByChecksum(ctx, checksum); a != nil {
		h.decorateFile(a)
		res.deduplicated = true
		return res.created(a)
	}

	asset, err := newUploadAsset(in.Filename, models.AssetTypeImage, "", session.UserID) // description filled by the job
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	asset.Origin, asset.OriginRef = in.Origin, in.OriginRef
	if in.AltText != "" {
		// A user-written alt text: process_image leaves it in place.
		asset.AltText, asset.AltTextEditedByUser = in.AltText, true
	}
	fileID, err := models.NewID()
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	storageKey := fmt.Sprintf("assets/%s/original%s", asset.ID, strings.ToLower(filepath.Ext(in.Filename)))
	// Width/Height/IsAnimated are stamped by the job from image-service.
	file := &models.AssetFile{
		ID:             fileID,
		AssetID:        asset.ID,
		OriginalName:   in.Filename,
		MimeType:       in.MimeType,
		SizeBytes:      int64(len(in.Raw)),
		S3Key:          storage.TenantKey(ctx, storageKey),
		ChecksumSHA256: checksum,
	}
	blob, err := ingest.PutBlob(ctx, h.storage, file.S3Key, in.Raw, in.MimeType)
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not store image")
	}
	stored, err := svc.Create(ctx, ingest.NewAsset{
		Asset: asset,
		File:  file,
		Blob:  blob,
		Enqueue: func(ctx context.Context, tx *sql.Tx) error {
			return h.imgJobs.EnqueueProcessImageTx(ctx, tx, asset.ID, session.TenantID, in.Filename, in.MimeType, storageKey, "run-1", "")
		},
	})
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}
	if stored == asset {
		asset.File = file
	} else {
		res.deduplicated = true // a concurrent upload of the same bytes won
	}
	h.decorateFile(stored)
	return res.created(stored)
}

// uploadFailure is a per-file reject: a stable code and its message.
type uploadFailure struct{ code, msg string }

// readImageUpload validates an image upload, then reads the original bytes,
// returning the MIME stored for it.
func (h *AssetsHandler) readImageUpload(fh *multipart.FileHeader) (string, []byte, *uploadFailure) {
	mimeType, fail := h.checkImageUpload(fh.Filename, fh.Size)
	if fail != nil {
		return "", nil, fail
	}
	raw, err := readFormFile(fh, maxImageUploadBytes())
	if err != nil {
		return "", nil, &uploadFailure{models.UploadCodeInternalError, "could not read file"}
	}
	if len(raw) == 0 {
		return "", nil, &uploadFailure{models.UploadCodeEmptyFile, "file is empty"}
	}
	return mimeType, raw, nil
}

// checkImageUpload validates an image's type (by filename extension), the
// service wiring and its size before its bytes are read, returning the MIME
// stored for it.
func (h *AssetsHandler) checkImageUpload(filename string, size int64) (string, *uploadFailure) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".svg" {
		return "", &uploadFailure{models.UploadCodeVectorRejected, "SVG / vector images are not supported — upload a raster image (JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, or BMP)"}
	}
	mimeType, ok := imageUploadMIMEs[ext]
	if !ok {
		return "", &uploadFailure{models.UploadCodeUnsupportedMediaType, "unsupported image type"}
	}
	// image-service is a hard dependency: with imgJobs nil there is no local
	// validation fallback.
	if h.storage == nil || h.db == nil || h.imgJobs == nil {
		return "", &uploadFailure{models.UploadCodeServiceUnavailable, "image processing is not configured"}
	}
	if size > maxImageUploadBytes() {
		return "", &uploadFailure{models.UploadCodeTooLarge, fmt.Sprintf("file exceeds maximum size of %d MB", maxImageUploadBytes()>>20)}
	}
	return mimeType, nil
}

func readFormFile(fh *multipart.FileHeader, limit int64) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit+1))
}

type createURLAssetRequest struct {
	URL string `json:"url" validate:"required"`
}

// CreateURL godoc
// @Summary      Create asset from a URL
// @Description  Scrapes a web page to Markdown (with mirrored images) via Firecrawl and persists it as a URL asset (CON-222). Ingestion is asynchronous: the asset is created `pending` and a background job scrapes → embeds → flips status, publishing progress over `GET /api/events` (topic `entity:asset:<id>`). Re-submitting the same URL refreshes the existing asset in place (200) instead of duplicating it (201).
// @Tags         content-bank
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      createURLAssetRequest  true  "URL payload"
// @Success      201   {object}  models.Asset  "new pending asset"
// @Success      200   {object}  models.Asset  "existing asset, refresh re-enqueued"
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      409   {object}  map[string]string  "URL scraping not configured"
// @Router       /api/content-bank/assets/url [post]
func (h *AssetsHandler) CreateURL(c *fiber.Ctx) error {
	var req createURLAssetRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	normalized, err := allowedSourceURL(reqCtx(c), req.URL)
	if err != nil {
		return err
	}

	// URL ingestion needs the enqueuer, the DB (transactional outbox), and a
	// configured Firecrawl key; otherwise fail fast so the caller isn't left
	// polling a pending asset that will never process.
	if !h.urlScrapeConfigured(reqCtx(c)) {
		return fiber.NewError(fiber.StatusConflict, "url scraping is not configured")
	}

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	ctx := reqCtx(c)

	// Dedupe: an existing URL asset with this source_url refreshes in place.
	existing, err := h.repo.GetBySourceURL(ctx, normalized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil {
		return h.refreshURLAsset(c, existing, normalized, session.TenantID)
	}

	// A genuinely new URL asset counts against BOTH the total bank
	// (content_bank_assets) and the stricter URL-only sub-cap (web_page_imports);
	// a refresh of an existing one (handled above) does not. The URL sub-cap is
	// checked first, so a workspace with bank room left but out of URL imports is
	// refused on the allowance that actually ran out.
	importQuota, err := requireQuota(c, h.limiter, "web_page_imports")
	if err != nil {
		return err
	}
	urlQuota, err := requireQuota(c, h.limiter, "content_bank_assets")
	if err != nil {
		return err
	}

	asset, err := newUploadAsset("", models.AssetTypeURL, "", session.UserID)
	if err != nil {
		return err
	}
	asset.Title = normalized // provisional; the worker sets the page title
	asset.SourceURL = new(normalized)
	if _, err := h.ingester().Create(ctx, ingest.NewAsset{
		Asset: asset,
		Enqueue: func(ctx context.Context, tx *sql.Tx) error {
			return h.urlJobs.EnqueueProcessURLTx(ctx, tx, asset.ID, session.TenantID, normalized, false)
		},
	}); err != nil {
		// Concurrent submit of the same new URL: the partial unique index rejects
		// the second insert. Treat it as "already exists" and refresh in place so
		// the call is idempotent.
		if again, gErr := h.repo.GetBySourceURL(ctx, normalized); gErr == nil && again != nil {
			return h.refreshURLAsset(c, again, normalized, session.TenantID)
		}
		return err
	}
	// The URL asset now exists — fire any near-limit crossing on both
	// the URL sub-cap and the total bank.
	importQuota.dispatch(ctx)
	urlQuota.dispatch(ctx)

	return c.Status(fiber.StatusCreated).JSON(asset)
}

// urlScrapeConfigured reports whether URL ingestion can run: it needs the
// enqueuer, the DB (transactional outbox), and a configured Firecrawl key.
func (h *AssetsHandler) urlScrapeConfigured(ctx context.Context) bool {
	return h.urlJobs != nil && h.db != nil && h.scrapeGate != nil && h.scrapeGate.HasKey(ctx)
}

// allowedSourceURL normalizes a submitted URL and runs the SSRF pre-flight
// (defense in depth; the worker's image fetcher is the authoritative
// connect-time guard): a host resolving to a private/loopback/link-local/
// metadata address is rejected. The lookup is bounded so a slow resolver
// can't stall the request.
func allowedSourceURL(ctx context.Context, raw string) (string, error) {
	normalized, host, err := normalizeSourceURL(raw)
	if err != nil {
		return "", fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := netguard.ResolveAllowed(lookupCtx, host); err != nil {
		return "", fiber.NewError(fiber.StatusBadRequest, "url host is not allowed")
	}
	return normalized, nil
}

// refreshURLAsset resets an existing URL asset to pending and re-enqueues a
// scrape (refresh=true), returning it with 200. Content/images/chunks are
// replaced by the worker; id/created_at/tags are preserved.
func (h *AssetsHandler) refreshURLAsset(c *fiber.Ctx, asset *models.Asset, sourceURL, tenantID string) error {
	ctx := reqCtx(c)
	if err := h.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().
			Model((*models.Asset)(nil)).
			Set("status = ?", models.AssetStatusPending).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", asset.ID).
			Exec(ctx); err != nil {
			return err
		}
		return h.urlJobs.EnqueueProcessURLTx(ctx, tx.Tx, asset.ID, tenantID, sourceURL, true)
	}); err != nil {
		return err
	}
	asset.Status = models.AssetStatusPending
	h.decorateFile(asset)
	h.decorateImages(reqCtx(c), asset)
	return c.Status(fiber.StatusOK).JSON(asset)
}

// normalizeSourceURL validates and canonicalises a submitted URL for dedupe:
// http(s) only, lower-cased scheme+host, fragment stripped, trailing slash
// trimmed (query preserved). It returns the normalised URL and its host (for
// the caller's SSRF resolution check). Host-level SSRF filtering is applied by
// the caller via netguard.
func normalizeSourceURL(raw string) (normalized, host string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid url")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("only http(s) urls are supported")
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("url is missing a host")
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path != "/" {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String(), netguard.NormalizeHost(u.Hostname()), nil
}

// Get godoc
// @Summary      Get asset
// @Description  Returns a single content bank asset by Sqid.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Asset Sqid"
// @Success      200  {object}  models.Asset
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/content-bank/assets/{id} [get]
func (h *AssetsHandler) Get(c *fiber.Ctx) error {
	asset, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "asset not found")
	}
	h.decorateFile(asset)
	h.decorateImages(reqCtx(c), asset)
	return c.JSON(asset)
}

// Update godoc
// @Summary      Update asset
// @Description  Whole-resource write of an asset's title, content, and (when present) alt_text / tag_ids — an omitted alt_text or tag_ids keeps the stored value.
// @Description  Content rules by type: MD/URL require non-empty content and re-embed on a title/content change. IMG content is the image description — may be empty; an edit re-embeds it together with the stored region chunks. PDF/DOC/AUDIO content is ingestion output and read-only: send it unchanged or omit it (empty = keep); a different value is a 409 `{code: "content_locked"}` — re-extract instead. Renaming an ingested asset never re-chunks it.
// @Description  `alt_text_edited_by_user` flips to true only when alt_text actually changes.
// @Tags         content-bank
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string             true  "Asset Sqid"
// @Param        body  body      updateAssetRequest true  "Asset payload"
// @Success      200   {object}  models.Asset
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string  "content_locked: content edit on a PDF/DOC/AUDIO asset"
// @Router       /api/content-bank/assets/{id} [put]
func (h *AssetsHandler) Update(c *fiber.Ctx) error {
	var req updateAssetRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	asset, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "asset not found")
	}

	content, err := ingest.EditContent(asset, req.Content)
	switch {
	case errors.Is(err, ingest.ErrContentLocked):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"code":  models.AssetCodeContentLocked,
			"error": err.Error(),
		})
	case errors.Is(err, ingest.ErrContentRequired):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	req.Content = content

	// alt_text and tag_ids are optional on the PUT: a nil pointer means
	// the caller didn't send the field, so its stored value is kept; a present
	// value — including "" or [] — replaces it. This is what stops a
	// title/content-only save from clearing tags or alt text.
	altText := asset.AltText
	if req.AltText != nil {
		altText, err = normalizeAltText(*req.AltText)
		if err != nil {
			return err
		}
	}

	reembed := ingest.ReembedAfterEdit(asset, req.Title, req.Content)

	// An alt_text edit marks the text hand-written so a later image
	// re-extraction never overwrites it. Only a real change counts: a
	// client echoing the stored value back must not lock generated text.
	if altText != asset.AltText {
		asset.AltTextEditedByUser = true
	}
	asset.Title = req.Title
	asset.Content = req.Content
	asset.AltText = altText
	if req.TagIDs != nil {
		asset.TagIDs = nullSlice(*req.TagIDs)
	}
	asset.UpdatedAt = time.Now().UTC()

	if err := h.repo.Update(reqCtx(c), asset); err != nil {
		return err
	}

	h.reembed(c, asset, reembed)

	h.decorateFile(asset)
	return c.JSON(asset)
}

// reembed refreshes an edited asset's embeddings as decided by
// ingest.ReembedAfterEdit: an image description goes through the image
// pipeline (keeping region chunks), an authored asset through onSave.
func (h *AssetsHandler) reembed(c *fiber.Ctx, asset *models.Asset, how ingest.Reembed) {
	tid, _ := tenantctx.From(reqCtx(c))
	switch {
	case how == ingest.ReembedImage && h.imgReembed != nil:
		if err := h.imgReembed.EnqueueReembedImage(reqCtx(c), asset.ID, tid); err != nil {
			slog.ErrorContext(reqCtx(c), "enqueue image re-embed failed", logging.AttrComponent, "handlers.assets", "asset_id", asset.ID, logging.AttrError, err)
		}
	case how == ingest.ReembedText && h.onSave != nil:
		backgroundTasks.Go("assets.on_save", func() { h.onSave(asset.ID, asset.Title, asset.Content, tid) })
	}
}

// bulkTagRequest is the payload for the bulk tag operation: the assets to touch
// and the tag IDs to add and/or remove on each. At least one of add/remove must
// be non-empty (enforced in the handler, not by a struct tag, so the message can
// name the actual rule).
type bulkTagRequest struct {
	AssetIDs []string `json:"asset_ids" validate:"required,min=1"`
	Add      []string `json:"add"`
	Remove   []string `json:"remove"`
}

// BulkTag godoc
// @Summary      Bulk add/remove tags across assets
// @Description  Adds and/or removes tag IDs on many assets in one call — the
// @Description  filing operation the Content Bank's multi-select needs (CON-279),
// @Description  where you tag many documents at once. Each listed asset keeps the
// @Description  tags it already has, minus `remove`, plus `add`; the result is
// @Description  deduped and preserves order. Assets outside the caller's tenant
// @Description  are skipped. Returns the updated assets with tags hydrated.
// @Tags         content-bank
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      bulkTagRequest  true  "Bulk tag payload"
// @Success      200   {array}   models.Asset
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/content-bank/assets/tags [post]
func (h *AssetsHandler) BulkTag(c *fiber.Ctx) error {
	var req bulkTagRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	if len(req.Add) == 0 && len(req.Remove) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "add or remove must name at least one tag")
	}
	// A tag named in both lists is contradictory — reject it rather than let the
	// merge pick a silent winner.
	for _, id := range req.Add {
		if slices.Contains(req.Remove, id) {
			return fiber.NewError(fiber.StatusBadRequest, "a tag cannot be both added and removed")
		}
	}

	updated, err := h.repo.ApplyTags(reqCtx(c), req.AssetIDs, req.Add, req.Remove)
	if err != nil {
		if errors.Is(err, repository.ErrUnknownTag) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}
	for i := range updated {
		h.decorateFile(&updated[i])
	}
	return c.JSON(updated)
}

// Delete godoc
// @Summary      Delete asset
// @Description  Deletes a content bank asset by Sqid.
// @Tags         content-bank
// @Security     CookieAuth
// @Param        id   path  string  true  "Asset Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/content-bank/assets/{id} [delete]
func (h *AssetsHandler) Delete(c *fiber.Ctx) error {
	id := c.Params("id")

	// Capture the object keys before the row goes: the cascade drops the file
	// and image rows that name them.
	keysToDelete, err := h.ingester().ObjectKeys(reqCtx(c), id)
	if err != nil {
		return err
	}

	deleted, err := h.repo.Delete(reqCtx(c), id)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "asset not found")
	}

	// Best-effort S3 cleanup. Logged but not surfaced — the row is gone
	// and orphaned objects are tolerable.
	if h.storage != nil {
		for _, k := range keysToDelete {
			if err := h.storage.Delete(reqCtx(c), k); err != nil {
				slog.WarnContext(reqCtx(c), "delete asset object", logging.AttrComponent, "handlers.assets",
					"key", k, logging.AttrError, err)
			}
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}
