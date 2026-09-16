package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/netguard"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const (
	maxMarkdownUploadSize = 10 << 20 // 10 MB
	maxPDFUploadSize      = 50 << 20 // 50 MB
	// maxDocumentUploadSize caps office/text document uploads (CON-280). Larger
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

// PDFIngestEnqueuer enqueues a PDF-ingestion job in the caller's transaction
// (CON-103). Implemented by *queues.Enqueuer; a narrow interface here keeps the
// handler off the jobs package.
type PDFIngestEnqueuer interface {
	EnqueueProcessPDFTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType string) error
}

// URLIngestEnqueuer enqueues a URL-scrape job in the caller's transaction
// (CON-222). Implemented by *queues.Enqueuer.
type URLIngestEnqueuer interface {
	EnqueueProcessURLTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, sourceURL string, refresh bool) error
}

// DocumentIngestEnqueuer enqueues a document-ingestion job in the caller's
// transaction (CON-280). Implemented by *queues.Enqueuer; a narrow interface
// here keeps the handler off the jobs package.
type DocumentIngestEnqueuer interface {
	EnqueueProcessDocumentTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey string) error
}

// ImageIngestEnqueuer enqueues an image-ingestion job in the caller's
// transaction (CON-281). Implemented by *queues.Enqueuer; a narrow interface
// here keeps the handler off the jobs package. Nil (no IMAGE_SERVICE_ADDR) makes
// image uploads fail fast — image-service is a hard dependency (D6).
type ImageIngestEnqueuer interface {
	EnqueueProcessImageTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error
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

	// onSave triggers async embedding for text-based Asset saves (JSON create/update + MD upload).
	onSave func(assetID, title, content, tenantID string)
	// pdfJobs enqueues PDF ingestion (CON-103). Nil disables it (the asset is
	// created but left pending).
	pdfJobs PDFIngestEnqueuer
	// urlJobs enqueues URL scraping (CON-222); scrapeGate reports key presence.
	// Nil urlJobs / scrapeGate makes the URL endpoint return 409.
	urlJobs    URLIngestEnqueuer
	scrapeGate URLScrapeGate
	// docJobs enqueues document ingestion (CON-280). Nil makes document uploads
	// fail fast with a "not configured" message.
	docJobs DocumentIngestEnqueuer
	// imgJobs enqueues image ingestion (CON-281). Nil makes image uploads fail
	// fast — image-service is a hard dependency (imageprobe was deleted, D6).
	imgJobs ImageIngestEnqueuer
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
) *AssetsHandler {
	return &AssetsHandler{
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
// empty (CON-246 R9); Update enforces it for the document types that still need
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
	assets, err := h.repo.List(c.Context())
	if err != nil {
		return err
	}
	for i := range assets {
		h.decorateFile(&assets[i])
	}
	h.decorateImagesBatch(c.Context(), assets)
	return c.JSON(assets)
}

// decorateImagesBatch hydrates mirrored images for a list of assets in one query
// (CON-222), URL-decorating each. No-op when the image repo is unwired.
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

// decorateFile fills File.URL (the original) and File.ThumbnailURL from their
// s3 keys using the public storage URL, when present. URL is what an image
// viewer renders (CON-246); ThumbnailURL is the PDF/first-page preview and, in
// future, the image grid thumbnail.
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
}

// decorateImages hydrates a URL asset's mirrored images (CON-222) and fills each
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

	altText, err := normalizeAltText(req.AltText)
	if err != nil {
		return err
	}

	session := c.Locals("session").(*models.Session)

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
	if err := h.repo.Create(c.Context(), asset); err != nil {
		return err
	}

	if h.onSave != nil {
		tid, _ := tenantctx.From(c.Context())
		go h.onSave(asset.ID, asset.Title, asset.Content, tid)
	}

	return c.Status(fiber.StatusCreated).JSON(asset)
}

// uploadResult is the per-file outcome in a batch upload.
type uploadResult struct {
	Filename string `json:"filename"`
	AssetID  string `json:"asset_id,omitempty"`
	Status   string `json:"status"` // "created" | "failed"
	Error    string `json:"error,omitempty"`
	// Code is a stable, machine-readable companion to Error on a failed result
	// (CON-281): the client matches the code and falls back to the prose when it
	// is unknown. Empty on a created result. See models.UploadCode*.
	Code  string        `json:"code,omitempty"`
	Asset *models.Asset `json:"asset,omitempty"`
}

// fail stamps a terminal per-file outcome with a machine-readable code and its
// human-readable message (CON-281), keeping the two in lockstep at every
// rejection site.
func (r *uploadResult) fail(code, msg string) uploadResult {
	r.Status = "failed"
	r.Code = code
	r.Error = msg
	return *r
}

// Upload godoc
// @Summary      Upload Markdown, PDF, or image file(s)
// @Description  Accepts one or more `.md` (max 10 MB), `.pdf` (max 50 MB), or image (JPEG/PNG/WebP/GIF, max 10 MB) files. Markdown files are converted to BlockNote JSON synchronously; PDFs are uploaded to object storage and processed asynchronously (text extraction, page-aware chunking, embedding, thumbnail); images (CON-246) are probed and stored synchronously as `IMG` assets (`ready`), deduplicated within the tenant by checksum. Files are processed independently — one failure does not block others.
// @Tags         content-bank
// @Accept       multipart/form-data
// @Produce      json
// @Security     CookieAuth
// @Param        files  formData  file  true  "Markdown, PDF, or image file(s)"
// @Success      201    {object}  map[string]any
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

	session := c.Locals("session").(*models.Session)

	results := make([]uploadResult, 0, len(files))
	for _, fh := range files {
		res := uploadResult{Filename: fh.Filename}

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
		results = append(results, res)
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"results": results})
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
		// "vector not supported" reject (CON-281) rather than a generic message.
		return uploadKindImage
	}
	// Raster images (CON-281) — advisory extension routing only; image-service
	// sniffs the body's magic bytes authoritatively.
	if _, ok := imageUploadMIMEs[ext]; ok {
		return uploadKindImage
	}
	// Office/text documents (CON-280) — advisory extension routing only;
	// document-service sniffs the body authoritatively.
	if _, ok := documentUploadMIMEs[ext]; ok {
		return uploadKindDocument
	}
	return uploadKindUnknown
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

	content := string(raw)
	title := strings.TrimSuffix(filepath.Base(fh.Filename), filepath.Ext(fh.Filename))

	id, err := models.NewID()
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	mdType := models.AssetTypeMarkdown
	asset := &models.Asset{
		ID:        id,
		Title:     title,
		Content:   content,
		Status:    models.AssetStatusPending,
		Type:      &mdType,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}
	if err := h.repo.Create(c.Context(), asset); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}

	if h.onSave != nil {
		tid, _ := tenantctx.From(c.Context())
		go h.onSave(asset.ID, asset.Title, asset.Content, tid)
	}

	res.AssetID = asset.ID
	res.Status = "created"
	res.Asset = asset
	return res
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

	title := strings.TrimSuffix(filepath.Base(fh.Filename), filepath.Ext(fh.Filename))
	id, err := models.NewID()
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	pdfType := models.AssetTypePDF
	asset := &models.Asset{
		ID:        id,
		Title:     title,
		Content:   "[]",
		Status:    models.AssetStatusPending,
		Type:      &pdfType,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}

	ctx := c.Context()

	// PDF ingestion (CON-103) needs object storage — the worker re-reads the PDF
	// from it on each attempt — plus the job enqueuer. Without them, create the
	// asset but skip processing (it stays pending).
	if h.storage == nil || h.pdfJobs == nil || h.db == nil {
		if err := h.repo.Create(ctx, asset); err != nil {
			return res.fail(models.UploadCodeInternalError, "could not create asset")
		}
		slog.WarnContext(c.Context(), "pdf ingestion disabled; asset left pending", logging.AttrComponent, "assets", "asset_id", asset.ID)
		res.AssetID = asset.ID
		res.Status = "created"
		res.Asset = asset
		return res
	}

	// 1. Store original.pdf BEFORE enqueue so the worker can re-read it on each
	//    attempt (the 50 MB bytes can't ride in the River job args).
	key := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/original.pdf", asset.ID))
	if _, err := h.storage.Upload(ctx, key, bytes.NewReader(raw), int64(len(raw)), "application/pdf"); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not store pdf")
	}

	// 2. Insert the asset and enqueue the ingestion job atomically (transactional
	//    outbox): a committed asset always has a job, a rolled-back one never does.
	if err := h.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(asset).Exec(ctx); err != nil {
			return err
		}
		return h.pdfJobs.EnqueueProcessPDFTx(ctx, tx.Tx, asset.ID, session.TenantID, fh.Filename, "application/pdf")
	}); err != nil {
		// The transaction rolled back, so the asset/job never persisted — delete
		// the original.pdf uploaded above (same key) so it isn't left orphaned in
		// the bucket. Best-effort: the failed upload is what the caller acts on.
		_ = h.storage.Delete(ctx, key)
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}

	res.AssetID = asset.ID
	res.Status = "created"
	res.Asset = asset
	return res
}

// processDocumentUpload ingests an office/text document (CON-280): store the
// original in object storage, then insert the asset and enqueue the extraction
// job atomically. Mirrors processPDFUpload. document-service does the
// authoritative format detection, so the handler only does a light OLE2 reject
// (the common legacy-.doc / encrypted-container case) for fast, clear feedback;
// everything else is sniffed by the service. Bytes land at
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

	// Document ingestion (CON-280) needs object storage (the worker re-reads the
	// file on each attempt), the job enqueuer, and the DB. server.go leaves
	// docJobs nil when DOCUMENTS_SERVICE_ADDR is empty, so a doc upload then fails
	// fast with a clear message — before reading the body into memory — instead of
	// stranding a pending asset (AC6).
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
	// Light early reject: OLE2 is both the legacy binary container (.doc/.xls/.ppt)
	// and the wrapper for password-protected OOXML — neither is supported.
	// document-service would reject it too, but catching it here is faster and
	// clearer.
	if isOLE2(raw) {
		return res.fail(models.UploadCodeUnsupportedMediaType, "legacy binary or password-protected Office files are not supported — save as unprotected .docx/.xlsx/.pptx and re-upload")
	}

	title := strings.TrimSuffix(filepath.Base(fh.Filename), filepath.Ext(fh.Filename))
	id, err := models.NewID()
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	docType := models.AssetTypeDocument
	asset := &models.Asset{
		ID:        id,
		Title:     title,
		Content:   "[]",
		Status:    models.AssetStatusPending,
		Type:      &docType,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}

	ctx := c.Context()

	// 1. Store original.<ext> BEFORE enqueue so the worker can re-read it on each
	//    attempt (the bytes can't ride in the River job args). storageKey is the
	//    tenant-relative path the worker resolves via storage.TenantKey.
	storageKey := fmt.Sprintf("assets/%s/original%s", asset.ID, ext)
	fullKey := storage.TenantKey(ctx, storageKey)
	if _, err := h.storage.Upload(ctx, fullKey, bytes.NewReader(raw), int64(len(raw)), mimeType); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not store document")
	}

	// 2. Insert the asset and enqueue ingestion atomically (transactional outbox):
	//    a committed asset always has a job, a rolled-back one never does.
	if err := h.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(asset).Exec(ctx); err != nil {
			return err
		}
		return h.docJobs.EnqueueProcessDocumentTx(ctx, tx.Tx, asset.ID, session.TenantID, fh.Filename, mimeType, storageKey)
	}); err != nil {
		// Rolled back — delete the orphaned original uploaded above. Best-effort.
		_ = h.storage.Delete(ctx, fullKey)
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}

	res.AssetID = asset.ID
	res.Status = "created"
	res.Asset = asset
	return res
}

// isOLE2 reports whether b starts with the OLE2 compound-file magic
// (D0 CF 11 E0 A1 B1 1A E1) used by legacy binary Office files (.doc/.xls/.ppt)
// and by the encrypted-OOXML container — neither is supported (CON-280).
func isOLE2(b []byte) bool {
	const sig = "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"
	return len(b) >= len(sig) && string(b[:len(sig)]) == sig
}

// processImageUpload ingests a content-bank image asset (CON-281). image-service
// is the single image authority, so ingestion is now ASYNC (mirroring PDF/audio/
// document): the handler stores the original, creates a `pending` IMG asset +
// file row, and enqueues a `process_image` job that normalizes, classifies,
// extracts, describes, alt-texts, and embeds it. The old synchronous imageprobe
// path is gone (D6) — with imageprobe deleted there is no pure-Go fallback, so an
// unwired service (imgJobs nil) fails the upload with a clear message rather than
// degrading. ogen still computes the SHA-256 itself (a plain hash of bytes, not
// image logic) so upload-time dedupe survives the move to async. Bytes land at
// assets/{id}/original.<ext>.
func (h *AssetsHandler) processImageUpload(c *fiber.Ctx, fh *multipart.FileHeader, session *models.Session) uploadResult {
	res := uploadResult{Filename: fh.Filename}

	ext := strings.ToLower(filepath.Ext(fh.Filename))
	// SVG / vector is a terminal reject with a specific message (CON-281 §18).
	if ext == ".svg" {
		return res.fail(models.UploadCodeVectorRejected, "SVG / vector images are not supported — upload a raster image (JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, or BMP)")
	}
	mimeType, ok := imageUploadMIMEs[ext]
	if !ok {
		// detectUploadKind already gated this; stay defensive.
		return res.fail(models.UploadCodeUnsupportedMediaType, "unsupported image type")
	}

	// Image ingestion needs object storage, the DB, and the job enqueuer. D6: with
	// imageprobe deleted, an empty IMAGE_SERVICE_ADDR (imgJobs nil) means uploads
	// are rejected — there is no local validation fallback.
	if h.storage == nil || h.db == nil || h.imgJobs == nil {
		return res.fail(models.UploadCodeServiceUnavailable, "image processing is not configured")
	}
	if fh.Size > maxImageUploadBytes() {
		return res.fail(models.UploadCodeTooLarge, fmt.Sprintf("file exceeds maximum size of %d MB", maxImageUploadBytes()>>20))
	}

	raw, err := readFormFile(fh, maxImageUploadBytes())
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not read file")
	}
	if len(raw) == 0 {
		return res.fail(models.UploadCodeEmptyFile, "file is empty")
	}

	ctx := c.Context()

	// Dedupe within the tenant by original-bytes checksum (R-Dedup): the same image
	// uploaded twice returns the first asset instead of a near-duplicate. The hash
	// is over the ORIGINAL bytes so it is stable regardless of later normalization.
	sum := sha256.Sum256(raw)
	checksum := hex.EncodeToString(sum[:])
	if h.fileRepo != nil {
		if existing, derr := h.fileRepo.GetByChecksum(ctx, checksum); derr == nil && existing != nil {
			if a, aerr := h.repo.GetByID(ctx, existing.AssetID); aerr == nil && a != nil {
				h.decorateFile(a)
				res.AssetID = a.ID
				res.Status = "created"
				res.Asset = a
				return res
			}
		}
	}

	title := strings.TrimSuffix(filepath.Base(fh.Filename), filepath.Ext(fh.Filename))
	id, err := models.NewID()
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}
	fileID, err := models.NewID()
	if err != nil {
		return res.fail(models.UploadCodeInternalError, "could not generate id")
	}

	imgType := models.AssetTypeImage
	asset := &models.Asset{
		ID:        id,
		Title:     title,
		Content:   "", // the image's description; filled by the job (empty is valid)
		Status:    models.AssetStatusPending,
		Type:      &imgType,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}
	storageKey := fmt.Sprintf("assets/%s/original%s", asset.ID, ext)
	fullKey := storage.TenantKey(ctx, storageKey)
	// Width/Height/IsAnimated are stamped by the job from image-service; the
	// checksum (computed here) backs dedupe and is stable across normalization.
	file := &models.AssetFile{
		ID:             fileID,
		AssetID:        asset.ID,
		OriginalName:   fh.Filename,
		MimeType:       mimeType,
		SizeBytes:      int64(len(raw)),
		S3Key:          fullKey,
		ChecksumSHA256: checksum,
	}

	// Store the original before the rows exist so a failed upload never leaves a
	// row pointing at missing bytes.
	if _, err := h.storage.Upload(ctx, fullKey, bytes.NewReader(raw), int64(len(raw)), mimeType); err != nil {
		return res.fail(models.UploadCodeInternalError, "could not store image")
	}

	// Insert the asset + file row and enqueue the ingestion job atomically
	// (transactional outbox): a committed asset always has its file and a job, a
	// rolled-back one leaves neither. On rollback, delete the blob so it isn't
	// orphaned.
	if err := h.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(asset).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(file).Exec(ctx); err != nil {
			return err
		}
		return h.imgJobs.EnqueueProcessImageTx(ctx, tx.Tx, asset.ID, session.TenantID, fh.Filename, mimeType, storageKey, "run-1", "")
	}); err != nil {
		_ = h.storage.Delete(ctx, fullKey)
		// Concurrent identical upload: the pre-check missed but the unique checksum
		// index rejected this second insert. Treat it as dedupe so the upload stays
		// idempotent instead of returning a spurious failure.
		if isUniqueViolationOn(err, "idx_asset_files_tenant_checksum") && h.fileRepo != nil {
			if existing, derr := h.fileRepo.GetByChecksum(ctx, checksum); derr == nil && existing != nil {
				if a, aerr := h.repo.GetByID(ctx, existing.AssetID); aerr == nil && a != nil {
					h.decorateFile(a)
					res.AssetID = a.ID
					res.Status = "created"
					res.Asset = a
					return res
				}
			}
		}
		return res.fail(models.UploadCodeInternalError, "could not create asset")
	}

	asset.File = file
	h.decorateFile(asset)
	res.AssetID = asset.ID
	res.Status = "created"
	res.Asset = asset
	return res
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
	normalized, host, err := normalizeSourceURL(req.URL)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	// SSRF pre-flight (defense in depth; the worker's image fetcher is the
	// authoritative connect-time guard). Reject a submission whose host resolves
	// to a private/loopback/link-local/metadata address. Bounded so a slow
	// resolver can't stall the request.
	lookupCtx, cancel := context.WithTimeout(c.Context(), 3*time.Second)
	defer cancel()
	if err := netguard.ResolveAllowed(lookupCtx, host); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "url host is not allowed")
	}

	// URL ingestion needs the enqueuer, the DB (transactional outbox), and a
	// configured Firecrawl key; otherwise fail fast so the caller isn't left
	// polling a pending asset that will never process.
	if h.urlJobs == nil || h.db == nil || h.scrapeGate == nil || !h.scrapeGate.HasKey(c.Context()) {
		return fiber.NewError(fiber.StatusConflict, "url scraping is not configured")
	}

	session := c.Locals("session").(*models.Session)
	ctx := c.Context()

	// Dedupe: an existing URL asset with this source_url refreshes in place.
	existing, err := h.repo.GetBySourceURL(ctx, normalized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil {
		return h.refreshURLAsset(c, existing, normalized, session.TenantID)
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}
	urlType := models.AssetTypeURL
	src := normalized
	asset := &models.Asset{
		ID:        id,
		Title:     normalized, // provisional; the worker sets the page title
		Content:   "",
		Status:    models.AssetStatusPending,
		Type:      &urlType,
		SourceURL: &src,
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: session.UserID,
	}
	if err := h.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(asset).Exec(ctx); err != nil {
			return err
		}
		return h.urlJobs.EnqueueProcessURLTx(ctx, tx.Tx, asset.ID, session.TenantID, normalized, false)
	}); err != nil {
		// Concurrent submit of the same new URL: the partial unique index rejects
		// the second insert. Treat it as "already exists" and refresh in place so
		// the call is idempotent.
		if again, gErr := h.repo.GetBySourceURL(ctx, normalized); gErr == nil && again != nil {
			return h.refreshURLAsset(c, again, normalized, session.TenantID)
		}
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(asset)
}

// refreshURLAsset resets an existing URL asset to pending and re-enqueues a
// scrape (refresh=true), returning it with 200. Content/images/chunks are
// replaced by the worker; id/created_at/tags are preserved.
func (h *AssetsHandler) refreshURLAsset(c *fiber.Ctx, asset *models.Asset, sourceURL, tenantID string) error {
	ctx := c.Context()
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
	h.decorateImages(c.Context(), asset)
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
	asset, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "asset not found")
	}
	h.decorateFile(asset)
	h.decorateImages(c.Context(), asset)
	return c.JSON(asset)
}

// Update godoc
// @Summary      Update asset
// @Description  Updates title and content of an existing asset.
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
// @Router       /api/content-bank/assets/{id} [put]
func (h *AssetsHandler) Update(c *fiber.Ctx) error {
	var req updateAssetRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	asset, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "asset not found")
	}

	// Content is required for document assets but optional for images, whose
	// description may be empty (CON-246 R9). The type is only known now, after
	// the load, which is why this isn't a struct-tag validation.
	isImage := asset.Type != nil && *asset.Type == models.AssetTypeImage
	if !isImage && strings.TrimSpace(req.Content) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "content is required")
	}

	// alt_text and tag_ids are optional on the PUT (CON-279): a nil pointer means
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

	// Detect whether the embedding inputs (title or content) actually changed,
	// so we don't re-embed an asset when only a tag or the alt text was toggled.
	embedInputChanged := asset.Title != req.Title || asset.Content != req.Content

	asset.Title = req.Title
	asset.Content = req.Content
	asset.AltText = altText
	// CON-281 D5: an explicit alt_text in the PUT is a manual edit — mark it so a
	// later image re-extraction never overwrites the user's wording.
	if req.AltText != nil {
		asset.AltTextEditedByUser = true
	}
	if req.TagIDs != nil {
		asset.TagIDs = nullSlice(*req.TagIDs)
	}
	asset.UpdatedAt = time.Now().UTC()

	if err := h.repo.Update(c.Context(), asset); err != nil {
		return err
	}

	if h.onSave != nil && embedInputChanged {
		tid, _ := tenantctx.From(c.Context())
		go h.onSave(asset.ID, asset.Title, asset.Content, tid)
	}

	h.decorateFile(asset)
	return c.JSON(asset)
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

	updated, err := h.repo.ApplyTags(c.Context(), req.AssetIDs, req.Add, req.Remove)
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

	// Collect S3 keys to clean up before deleting the row; DB cascade will
	// drop the asset_files row when we delete the asset, so capture now.
	var keysToDelete []string
	if h.fileRepo != nil {
		if f, err := h.fileRepo.GetByAssetID(c.Context(), id); err == nil && f != nil {
			if f.S3Key != "" {
				keysToDelete = append(keysToDelete, f.S3Key)
			}
			if f.ThumbnailS3Key != nil && *f.ThumbnailS3Key != "" {
				keysToDelete = append(keysToDelete, *f.ThumbnailS3Key)
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// CON-222: mirrored image blobs (the DB cascade drops asset_images rows).
	if h.imageRepo != nil {
		if imgs, err := h.imageRepo.GetByAssetID(c.Context(), id); err == nil {
			for i := range imgs {
				if imgs[i].S3Key != "" {
					keysToDelete = append(keysToDelete, imgs[i].S3Key)
				}
			}
		}
	}
	// CON-282: the audio normalized derivative lives at a deterministic key
	// alongside the original (evicted with the asset, D5). It has no DB row of
	// its own, so add it unconditionally — the best-effort Delete below is a
	// no-op when the object doesn't exist (non-audio assets).
	keysToDelete = append(keysToDelete, storage.TenantKey(c.Context(), fmt.Sprintf("assets/%s/normalized.opus", id)))
	// CON-281: the image normalized.png derivative also lives at a deterministic
	// key alongside the original with no DB row of its own; evict it with the
	// asset. The best-effort Delete below is a no-op for non-image assets.
	keysToDelete = append(keysToDelete, storage.TenantKey(c.Context(), fmt.Sprintf("assets/%s/normalized.png", id)))

	deleted, err := h.repo.Delete(c.Context(), id)
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
			if err := h.storage.Delete(c.Context(), k); err != nil {
				// Fiber has no handler-level logger dependency here; swallow.
				_ = err
			}
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}
