package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/storage/pdfprobe"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/usecase/ingest"
)

// The upload ceilings (image/pdf/video bytes, alt-text length) are not
// constants: they live in operator-controlled global config and are read
// through the accessors in global_limits.go.
const (
	// postAttachmentsPositionConstraint is the name Postgres gives the inline
	// UNIQUE (post_id, position) on post_attachments (baseline schema). Used to
	// scope the reorder 409 to that specific collision.
	postAttachmentsPositionConstraint = "post_attachments_post_id_position_key"

	// pdfThumbnailDPI is the resolution used when rendering the first-page
	// preview for PDF attachments.
	pdfThumbnailDPI = 96

	// pdfRenderTimeout caps the pdf-service Render call for an attachment, so a
	// slow or unreachable service never blocks the upload request for long — on
	// failure the attachment is created without page count / thumbnail.
	pdfRenderTimeout = 30 * time.Second

	// videoProbeTimeout caps the video-service Probe call at finalize. Header
	// probing is fast, but the service range-reads a remote URL, so a
	// generous cap absorbs seek/network latency without hanging the request.
	// On failure the attachment is created unprobed (no duration/poster).
	videoProbeTimeout = 60 * time.Second
)

// PDFRenderer renders an attachment PDF to a page count + first-page thumbnail
// via pdf-service. Implemented by *pdf.Client; an interface here
// keeps the handler testable and nil-tolerant (nil disables it).
type PDFRenderer interface {
	Render(ctx context.Context, r io.Reader, opts pdf.RenderOptions) (*pdf.RenderResult, error)
}

// ImagePreparer runs the image-service light path: validate + metadata
// + EXIF-strip with pixels preserved (PrepareAttachment), and async alt-text
// (GenerateAltText). Implemented by *imageclient.Client; a nil preparer disables
// image attachments — image-service is a hard dependency (imageprobe deleted, D6).
type ImagePreparer interface {
	PrepareAttachment(ctx context.Context, opts imageclient.PrepareAttachmentOptions) (*imageclient.PrepareAttachmentResult, error)
	GenerateAltText(ctx context.Context, opts imageclient.GenerateAltTextOptions) (*imageclient.GenerateAltTextResult, error)
}

// PresignedURLTTL controls how long pre-signed GET URLs returned in
// API responses stay valid. Short window keeps stale URLs out of
// caches; long enough that the editor UI has plenty of time to load
// the image once. Exposed as a var so integration tests can shrink
// the window to assert expiry behaviour without sleeping for minutes.
var PresignedURLTTL = 15 * time.Minute

// PostAttachmentsHandler exposes the upload/list/reorder/delete API
// for post attachments — images and PDFs. All
// mutations are blocked once the parent post is submitted — scheduled or
// published; see ensureMutable.
type PostAttachmentsHandler struct {
	repo     repository.PostAttachmentRepository
	postRepo repository.PostRepository
	storage  storage.Storage
	pdf      PDFRenderer
	video    VideoProber
	// image runs the CON-281 light path (EXIF-strip + metadata + async alt text).
	// Nil disables image attachments (image-service unwired, D6). recorder meters
	// the alt-text vision call (CON-86, nil-safe); altTextMaxChars is the
	// generation target length. The model is the vision/alt_text slot, resolved
	// per call.
	image           ImagePreparer
	recorder        *usage.Recorder
	altTextMaxChars int
	auth            fiber.Handler
	limiter         *entitlements.Limiter // CON-295 media_storage_bytes quota (nil-safe)
	// bank reads content-bank image assets for attach-from-asset. Nil disables
	// that endpoint (503); set via WithContentBank.
	bank BankAssetReader
}

func NewPostAttachmentsHandler(
	repo repository.PostAttachmentRepository,
	postRepo repository.PostRepository,
	store storage.Storage,
	renderer PDFRenderer,
	prober VideoProber,
	preparer ImagePreparer,
	recorder *usage.Recorder,
	altTextMaxChars int,
	auth fiber.Handler,
	limiter *entitlements.Limiter,
) *PostAttachmentsHandler {
	return &PostAttachmentsHandler{
		repo:            repo,
		postRepo:        postRepo,
		storage:         store,
		pdf:             renderer,
		video:           prober,
		image:           preparer,
		recorder:        recorder,
		altTextMaxChars: altTextMaxChars,
		auth:            auth,
		limiter:         limiter,
	}
}

func (h *PostAttachmentsHandler) Register(app *fiber.App) {
	g := app.Group("/api/posts/:post_id/attachments", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Upload)
	// Large-file video ingest: presign a direct-to-S3 PUT, then
	// finalize (probe + validate + persist). Static paths are registered
	// before the /:id param route so they aren't captured as id="presign".
	g.Post("/presign", h.PresignVideo)
	g.Post("/finalize", h.FinalizeVideo)
	g.Post("/from-asset", h.AttachFromAsset)
	// Static /reorder is registered before the /:id param route so a PATCH to.
	// ../attachments/reorder isn't captured as id="reorder".
	g.Patch("/reorder", h.ReorderAll)
	g.Get("/:id", h.Get)
	g.Patch("/:id", h.Update)
	g.Delete("/:id", h.Delete)
}

// attachmentResponse wraps a single attachment with the soft-validation
// block per CON-73 §2.4. ValidationErrors is empty when the attachment
// passes every rule for the post's currently-selected platform.
type attachmentResponse struct {
	*models.PostAttachment
	PlatformValidation []platforms.ValidationError `json:"platform_validation"`
}

// listResponse mirrors attachmentResponse but for list endpoints, with
// the post-level rules (e.g. count cap, mixed-kind warning) surfaced
// once at the top.
type listResponse struct {
	Attachments        []attachmentResponse        `json:"attachments"`
	PlatformValidation []platforms.ValidationError `json:"platform_validation"`
}

// hydratePresigned fills att.PresignedURL (and ThumbnailURL when a
// thumbnail key is present) when storage is configured. Errors are
// swallowed — a missing presigned URL still leaves callers with the
// metadata, and the binary is never the source of truth.
func (h *PostAttachmentsHandler) hydratePresigned(c *fiber.Ctx, att *models.PostAttachment) {
	if h.storage == nil || att == nil {
		return
	}
	if att.S3Key != "" {
		if url, err := h.storage.PresignedGetURL(reqCtx(c), att.S3Key, PresignedURLTTL); err == nil {
			att.PresignedURL = url
		}
	}
	if att.ThumbnailS3Key != "" {
		if url, err := h.storage.PresignedGetURL(reqCtx(c), att.ThumbnailS3Key, PresignedURLTTL); err == nil {
			att.ThumbnailURL = url
		}
	}
}

// List godoc
// @Summary      List post attachments
// @Description  Returns the post's attachments ordered by position, with a
// @Description  soft pre-check (`platform_validation`) per CON-73 §2.4 that
// @Description  surfaces any rule failure for the post's current target
// @Description  platform. Each attachment carries a short-lived presigned
// @Description  GET URL when object storage is configured.
// @Tags         post-attachments
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string  true  "Post Sqid"
// @Success      200      {object}  listResponse
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments [get]
func (h *PostAttachmentsHandler) List(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	atts, err := h.repo.ListByPostID(reqCtx(c), post.ID)
	if err != nil {
		return err
	}

	out := listResponse{
		Attachments:        make([]attachmentResponse, 0, len(atts)),
		PlatformValidation: platforms.ValidatePostAttachments(atts, post.Platform),
	}
	for i := range atts {
		h.hydratePresigned(c, &atts[i])
		out.Attachments = append(out.Attachments, attachmentResponse{
			PostAttachment:     &atts[i],
			PlatformValidation: platforms.ValidateAttachment(&atts[i], post.Platform),
		})
	}
	return c.JSON(out)
}

// Get godoc
// @Summary      Get post attachment
// @Description  Returns metadata for a single attachment plus its soft
// @Description  validation block and a short-lived presigned GET URL.
// @Tags         post-attachments
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string  true  "Post Sqid"
// @Param        id       path      string  true  "Attachment id"
// @Success      200      {object}  attachmentResponse
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/{id} [get]
func (h *PostAttachmentsHandler) Get(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	att, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "attachment not found")
	}
	if att.PostID != post.ID {
		return fiber.NewError(fiber.StatusNotFound, "attachment not found")
	}
	h.hydratePresigned(c, att)
	return c.JSON(attachmentResponse{
		PostAttachment:     att,
		PlatformValidation: platforms.ValidateAttachment(att, post.Platform),
	})
}

// rejectAttachment writes a terminal per-request attachment rejection carrying a
// stable, machine-readable code beside the human message. It mirrors
// the batch content-bank upload's {code,error} shape so the front-end can match
// on the code across both surfaces and fall back to the prose when it is
// unknown.
func rejectAttachment(c *fiber.Ctx, status int, code, msg string) error {
	return rejectCode(c, status, code, msg)
}

// imageRejectStatus is the HTTP status for a fine-grained image reject code
// (CON-281 Phase 2): a media type we don't accept (unsupported / vector) is 415;
// a typed-but-unusable image (too large / dimensions / corrupt) is 400.
func imageRejectStatus(code string) int {
	switch code {
	case models.UploadCodeUnsupportedMediaType, models.UploadCodeVectorRejected:
		return fiber.StatusUnsupportedMediaType
	default:
		return fiber.StatusBadRequest
	}
}

// Upload godoc
// @Summary      Upload a post attachment
// @Description  Accepts a single image (CON-73) or PDF (CON-75) file via
// @Description  multipart/form-data under the field `file`. Images — JPEG,
// @Description  PNG, WebP, GIF, HEIC/HEIF, AVIF, TIFF, BMP — are validated and
// @Description  EXIF-stripped (pixels preserved) by image-service (CON-281);
// @Description  SVG is rejected. PDFs are validated as application/pdf and
// @Description  get a best-effort first-page PNG thumbnail. Video uses the
// @Description  presign flow instead. Caps are operator-set (defaults: 50 MB
// @Description  images, 100 MB PDFs) and the upload counts toward the
// @Description  tenant's media_storage_bytes tier limit (402 when over).
// @Description  Per-platform caps are surfaced as soft warnings in the
// @Description  response. A reject is `{code, error}` with a stable code:
// @Description  unsupported_media_type, vector_rejected, too_large,
// @Description  empty_file, invalid_file, dimensions_exceeded,
// @Description  service_unavailable, internal_error.
// @Tags         post-attachments
// @Accept       multipart/form-data
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string  true  "Post Sqid"
// @Param        file     formData  file    true  "Image or PDF file"
// @Success      201      {object}  attachmentResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      402      {object}  map[string]any     "media_storage_bytes limit reached"
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string  "post is in a terminal publishing state"
// @Failure      415      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments [post]
func (h *PostAttachmentsHandler) Upload(c *fiber.Ctx) error {
	if h.storage == nil {
		return rejectAttachment(c, fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "storage not configured")
	}

	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if err := ensureMutable(post); err != nil {
		return err
	}

	up, err := h.openUpload(c)
	if err != nil {
		return attachmentError(c, err)
	}
	defer func() { _ = up.file.Close() }()

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	att, err := newUploadAttachment(c, post, session.UserID)
	if err != nil {
		return err
	}

	var thumbnail []byte
	if up.kind == pdfprobe.MIME {
		thumbnail, err = h.preparePDF(reqCtx(c), up, att)
	} else {
		err = h.prepareImage(reqCtx(c), up, att)
	}
	if err != nil {
		return attachmentError(c, err)
	}
	return h.createAttachment(c, post, att, thumbnail, up.quota, session.TenantID)
}

// createAttachment persists a prepared attachment and answers 201. Once the row
// and its bytes exist it fires any near-limit quota crossing, and for an image
// with no alt text it starts generation in the background: the request stays
// fast, and the generator writes only where alt text is still un-edited.
func (h *PostAttachmentsHandler) createAttachment(c *fiber.Ctx, post *models.Post, att *models.PostAttachment, thumbnail []byte, quota quotaHold, tenantID string) error {
	if err := h.persistAttachment(reqCtx(c), att, thumbnail); err != nil {
		return err
	}
	quota.dispatch(reqCtx(c))

	if strings.HasPrefix(att.MimeType, "image/") && att.AltText == "" && h.image != nil {
		altCtx := detachedContext(c, tenantID)
		backgroundTasks.Go("post_attachments.alt_text", func() {
			h.generateAttachmentAltText(altCtx, tenantID, att.ID, att.S3Key)
		})
	}

	h.hydratePresigned(c, att)
	return c.Status(fiber.StatusCreated).JSON(attachmentResponse{
		PostAttachment:     att,
		PlatformValidation: platforms.ValidateAttachment(att, post.Platform),
	})
}

// attachmentReject is a terminal upload rejection carrying a stable code; the
// handler renders it through rejectAttachment.
type attachmentReject struct {
	status int
	code   string
	msg    string
}

func (r *attachmentReject) Error() string { return r.msg }

func rejectUpload(status int, code, msg string) error {
	return &attachmentReject{status: status, code: code, msg: msg}
}

// attachmentError renders an *attachmentReject as its coded response and
// passes any other error through.
func attachmentError(c *fiber.Ctx, err error) error {
	if r, ok := errors.AsType[*attachmentReject](err); ok {
		return rejectAttachment(c, r.status, r.code, r.msg)
	}
	return err
}

// attachmentUpload is an opened, sniffed, quota-cleared upload.
type attachmentUpload struct {
	header *multipart.FileHeader
	file   multipart.File
	// kind is the sniffed content type (magic bytes, not the client's claim).
	kind  string
	quota quotaHold
}

// openUpload reads the form file, rejects anything above the larger of the
// per-kind caps before opening it (the precise cap is enforced per kind),
// gates media_storage_bytes before touching storage so a denied upload leaves
// no object, and sniffs the magic bytes. The caller closes the file.
func (h *PostAttachmentsHandler) openUpload(c *fiber.Ctx) (*attachmentUpload, error) {
	fh, err := c.FormFile("file")
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "file is required")
	}
	preSniffCap := max(maxPDFUploadBytes(), maxImageUploadBytes())
	if fh.Size > preSniffCap {
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeTooLarge,
			fmt.Sprintf("file exceeds upload limit of %d MB", preSniffCap>>20))
	}
	quota, err := requireQuotaAmount(c, h.limiter, "media_storage_bytes", fh.Size)
	if err != nil {
		return nil, err
	}

	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("post_attachments: open upload: %w", err)
	}
	sniff := make([]byte, 512)
	n, _ := io.ReadFull(f, sniff)
	if n == 0 {
		_ = f.Close()
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeEmptyFile, "file is empty")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("post_attachments: rewind upload: %w", err)
	}
	return &attachmentUpload{header: fh, file: f, kind: http.DetectContentType(sniff[:n]), quota: quota}, nil
}

// newUploadAttachment builds the attachment row from the form fields. On a
// thread post the client may name the (0-based) segment the media belongs to;
// blank means a whole-post attachment. The index is range-checked at the
// publish gate, since media may be uploaded before segments are set.
func newUploadAttachment(c *fiber.Ctx, post *models.Post, createdBy string) (*models.PostAttachment, error) {
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	altText, err := normalizeAltText(c.FormValue("alt_text"))
	if err != nil {
		return nil, err
	}
	segIdx, err := parseSegmentIndex(c.FormValue("segment_index"))
	if err != nil {
		return nil, err
	}
	if segIdx != nil && !post.IsThread() {
		return nil, fiber.NewError(fiber.StatusUnprocessableEntity, "segment_index is only valid on a thread post")
	}
	return &models.PostAttachment{
		ID:           id,
		PostID:       post.ID,
		AltText:      altText,
		SegmentIndex: segIdx,
		CreatedBy:    createdBy,
		// A user-supplied alt text on upload is a manual edit: the async
		// generator never overwrites it.
		AltTextEditedByUser: altText != "",
	}, nil
}

// attachmentKey is the tenant-scoped storage key of an attachment object.
func attachmentKey(ctx context.Context, att *models.PostAttachment, suffix string) string {
	return storage.TenantKey(ctx, "post-attachments/"+att.PostID+"/"+att.ID+suffix)
}

// preparePDF validates and stores a PDF upload, returning its rendered
// first-page thumbnail (nil when unavailable). Page count and thumbnail come
// from pdf-service best-effort; a terminal "not a usable PDF" verdict is a
// client error, since pdfprobe only checks the magic prefix.
func (h *PostAttachmentsHandler) preparePDF(ctx context.Context, up *attachmentUpload, att *models.PostAttachment) ([]byte, error) {
	if up.header.Size > maxPDFUploadBytes() {
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeTooLarge,
			fmt.Sprintf("PDF exceeds upload limit of %d MB", maxPDFUploadBytes()>>20))
	}
	probe, raw, err := pdfprobe.Probe(up.file, maxPDFUploadBytes())
	if err != nil {
		if errors.Is(err, pdfprobe.ErrUnsupportedMIME) {
			return nil, rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, err.Error())
		}
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeInvalidFile, err.Error())
	}
	att.MimeType = probe.MIME
	att.SizeBytes = probe.Size
	att.ChecksumSHA256 = probe.SHA256

	thumbnail, err := h.renderPDF(ctx, up.header.Filename, raw, att)
	if err != nil {
		return nil, err
	}
	att.S3Key = attachmentKey(ctx, att, probe.Extension)
	if _, err := h.storage.Upload(ctx, att.S3Key, bytes.NewReader(raw), att.SizeBytes, att.MimeType); err != nil {
		return nil, fmt.Errorf("post_attachments: storage upload: %w", err)
	}
	return thumbnail, nil
}

// renderPDF sets the page count (before platform soft-validation, so the
// max_pages check works) and returns the thumbnail. Transient or unreachable
// pdf-service failures degrade to no page count / thumbnail.
func (h *PostAttachmentsHandler) renderPDF(ctx context.Context, filename string, raw []byte, att *models.PostAttachment) ([]byte, error) {
	if h.pdf == nil {
		return nil, nil
	}
	rctx, cancel := context.WithTimeout(ctx, pdfRenderTimeout)
	render, err := h.pdf.Render(rctx, bytes.NewReader(raw), pdf.RenderOptions{
		RenderThumbnail: true,
		ThumbnailDPI:    pdfThumbnailDPI,
	})
	cancel()
	if err != nil {
		if pdf.IsInvalidPDF(err) {
			return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeInvalidFile, "uploaded file is not a readable PDF")
		}
		slog.WarnContext(ctx, "pdf render failed", logging.AttrComponent, "post_attachments", "name", filename, logging.AttrError, err)
		return nil, nil
	}
	att.PageCount = render.PageCount
	return render.ThumbnailPNG, nil
}

// prepareImage runs an image upload through image-service, the sole image
// authority: validate + metadata + EXIF-strip with pixels preserved. The
// EXIF-bearing original is staged at a temp key, image-service reads it via a
// presigned GET and writes the cleaned copy to the final key via a presigned
// PUT; the original is always discarded — its EXIF/geolocation must never
// persist or publish.
func (h *PostAttachmentsHandler) prepareImage(ctx context.Context, up *attachmentUpload, att *models.PostAttachment) error {
	fh := up.header
	if fh.Size > maxImageUploadBytes() {
		return rejectUpload(fiber.StatusBadRequest, models.UploadCodeTooLarge,
			fmt.Sprintf("image exceeds upload limit of %d MB", maxImageUploadBytes()>>20))
	}
	if h.image == nil {
		return rejectUpload(fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "image processing is not configured")
	}
	// The extension routes the object key + presign content type; the body is
	// still sniffed authoritatively by image-service.
	ext := strings.ToLower(filepath.Ext(fh.Filename))
	mime, ok := imageUploadMIMEs[ext]
	if !ok {
		if ext == ".svg" {
			return rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeVectorRejected, "SVG / vector images are not supported — upload a raster image (JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, or BMP)")
		}
		return rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "unsupported image type — accepted: JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, BMP")
	}
	raw, err := io.ReadAll(up.file)
	if err != nil {
		return fmt.Errorf("post_attachments: read image: %w", err)
	}

	cleanKey := attachmentKey(ctx, att, ext)
	orig, err := ingest.PutBlob(ctx, h.storage, attachmentKey(ctx, att, ".orig"+ext), raw, mime)
	if err != nil {
		return fmt.Errorf("post_attachments: stage original: %w", err)
	}
	defer orig.Discard(ctx)

	prep, err := h.stripImage(ctx, orig.Key(), cleanKey, mime, fh.Filename)
	if err != nil {
		return err
	}
	att.MimeType = prep.Mime
	att.SizeBytes = prep.SizeBytes
	att.Width = prep.Width
	att.Height = prep.Height
	att.IsAnimated = prep.IsAnimated
	att.ChecksumSHA256 = prep.ChecksumSHA256
	att.S3Key = cleanKey
	return nil
}

// stripImage hands image-service presigned GET/PUT URLs for the staged
// original and the clean key. On any failure the partial cleaned object is
// dropped too, so no orphaned bytes remain.
func (h *PostAttachmentsHandler) stripImage(ctx context.Context, origKey, cleanKey, mime, filename string) (*imageclient.PrepareAttachmentResult, error) {
	getURL, gerr := h.storage.PresignedGetURL(ctx, origKey, PresignedURLTTL)
	putURL, perr := h.storage.PresignedPutURL(ctx, cleanKey, mime, PresignedURLTTL)
	if gerr != nil || perr != nil {
		return nil, fmt.Errorf("post_attachments: presign image transfer: get=%v put=%v", gerr, perr)
	}
	prep, err := h.image.PrepareAttachment(ctx, imageclient.PrepareAttachmentOptions{
		SourceURL:     getURL,
		DestPutURL:    putURL,
		StripMetadata: true,
		WantAltText:   false, // alt text is generated asynchronously after upload
		Filename:      filename,
	})
	if err != nil {
		_ = h.storage.Delete(ctx, cleanKey)
		return nil, imagePrepareReject(err)
	}
	if prep.RejectedReason != "" {
		_ = h.storage.Delete(ctx, cleanKey)
		// A structured verdict from the service, carried with its own message.
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeInvalidFile, prep.RejectedReason)
	}
	return prep, nil
}

// imagePrepareReject maps a PrepareAttachment failure to its rejection. The
// fine-grained reason image-service attaches to a terminal reject wins; the
// coarse gRPC-code buckets are the fallback for a service that carries no
// ErrorInfo. Transient / unreachable failures are a retryable 503 (there is
// no local fallback).
func imagePrepareReject(err error) error {
	if code := imageclient.UploadCode(err); code != "" {
		return rejectUpload(imageRejectStatus(code), code, models.UploadRejectMessage(code))
	}
	switch {
	case imageclient.IsUnsupportedImage(err):
		return rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "unsupported image format")
	case imageclient.IsInvalidImage(err):
		return rejectUpload(fiber.StatusBadRequest, models.UploadCodeInvalidFile, "uploaded file is not a readable image")
	default:
		return rejectUpload(fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "image processing is temporarily unavailable; please retry")
	}
}

// persistAttachment stores the PDF thumbnail (best-effort — the attachment is
// valid without one) and inserts the row at the next position atomically. If
// the insert fails the stored objects are deleted: without the row they are
// undiscoverable dead weight.
func (h *PostAttachmentsHandler) persistAttachment(ctx context.Context, att *models.PostAttachment, thumbnail []byte) error {
	if len(thumbnail) > 0 {
		thumbKey := attachmentKey(ctx, att, ".thumb.png")
		if _, err := h.storage.Upload(ctx, thumbKey, bytes.NewReader(thumbnail), int64(len(thumbnail)), "image/png"); err == nil {
			att.ThumbnailS3Key = thumbKey
		}
	}
	if err := h.repo.CreateAtNextPosition(ctx, att); err != nil {
		_ = h.storage.Delete(ctx, att.S3Key)
		if att.ThumbnailS3Key != "" {
			_ = h.storage.Delete(ctx, att.ThumbnailS3Key)
		}
		return err
	}
	return nil
}

// generateAttachmentAltText runs image-service's GenerateAltText for a freshly
// uploaded image attachment and stores the result. Fire-and-forget:
// it runs in its own goroutine off a detached context, meters the vision call,
// and persists only where the user hasn't edited the alt text (D5).
// Best-effort throughout — a failure just leaves the attachment without alt text
// (regeneration is a separate, explicit action).
func (h *PostAttachmentsHandler) generateAttachmentAltText(ctx context.Context, tenantID, attID, s3Key string) {
	if h.image == nil || h.storage == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Rebuild the tenant context the request carried, so the presign + repo write
	// run against the right tenant.
	ctx = tenantctx.With(ctx, tenantID)

	getURL, err := h.storage.PresignedGetURL(ctx, s3Key, PresignedURLTTL)
	if err != nil {
		return
	}
	res, err := h.image.GenerateAltText(ctx, imageclient.GenerateAltTextOptions{
		SourceURL: getURL,
		MaxChars:  h.altTextMaxChars,
		Model:     modelconfig.Model(ctx, modelconfig.FlowVision, modelconfig.SlotAltText),
	})
	if err != nil || res == nil {
		slog.WarnContext(ctx, "attachment alt-text generation failed", logging.AttrComponent, "post_attachments", "attachment_id", attID, logging.AttrError, err)
		return
	}
	// Meter the vision call on the gemini vendor; RecordResp is nil-safe.
	for _, u := range res.Usage {
		h.recorder.RecordResp(ctx, llm.VendorGemini, u.Model, "alt_text", llm.VisionUsage{Step: u.Step, InputTokens: u.Input, OutputTokens: u.Output})
	}
	alt := strings.TrimSpace(res.AltText)
	if alt == "" {
		return
	}
	// Guard the storage cap (runes) — the generation target is short, but stay safe.
	alt = truncateRunes(alt, maxAltTextLen())
	if err := h.repo.SetGeneratedAltText(ctx, attID, alt); err != nil {
		slog.WarnContext(ctx, "attachment alt-text persist failed", logging.AttrComponent, "post_attachments", "attachment_id", attID, logging.AttrError, err)
	}
}

// parseSegmentIndex parses the optional segment_index form/JSON value for a
// thread attachment. A blank value yields nil (NULL — a whole-post
// attachment); a non-negative integer yields a pointer to it. A negative or
// non-numeric value is a 400.
func parseSegmentIndex(s string) (*int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return nil, fiber.NewError(fiber.StatusBadRequest, "segment_index must be a non-negative integer")
	}
	return &n, nil
}

// normalizeAltText trims and length-bounds accessibility alt text.
// The cap is in characters (runes), so multibyte alt text isn't rejected early.
func normalizeAltText(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxAltTextLen() {
		return "", fiber.NewError(fiber.StatusBadRequest,
			fmt.Sprintf("alt_text exceeds %d characters", maxAltTextLen()))
	}
	return s, nil
}

// updateAttachmentRequest is the PATCH body. Both fields are optional; at least
// one must be present. position reorders; alt_text sets accessibility
// text.
type updateAttachmentRequest struct {
	Position *int    `json:"position"`
	AltText  *string `json:"alt_text"`
	// SegmentIndex reassigns which thread segment the media belongs to.
	// Presence-aware so the three JSON states stay distinct: absent ⇒ leave as-is,
	// a number ⇒ move to that segment, explicit null ⇒ detach (back to a
	// whole-post attachment).
	SegmentIndex Optional[int] `json:"segment_index"`
}

// toPatch validates and normalizes every field before any mutation, so an
// invalid field can't leave a partially-applied update (e.g. position already
// changed).
func (req *updateAttachmentRequest) toPatch(post *models.Post) (repository.AttachmentPatch, error) {
	var patch repository.AttachmentPatch
	if req.Position == nil && req.AltText == nil && !req.SegmentIndex.Present {
		return patch, fiber.NewError(fiber.StatusBadRequest, "at least one of position, alt_text or segment_index is required")
	}
	if req.Position != nil && *req.Position < 0 {
		return patch, fiber.NewError(fiber.StatusBadRequest, "position must be non-negative")
	}
	if req.SegmentIndex.Present && req.SegmentIndex.Value != nil {
		if *req.SegmentIndex.Value < 0 {
			return patch, fiber.NewError(fiber.StatusBadRequest, "segment_index must be non-negative")
		}
		// A non-null segment only belongs on a thread post; an explicit null is
		// always allowed (it clears the field back to a whole-post attachment).
		if !post.IsThread() {
			return patch, fiber.NewError(fiber.StatusUnprocessableEntity, "segment_index is only valid on a thread post")
		}
	}
	patch.Position = req.Position
	patch.SetSegmentIndex = req.SegmentIndex.Present
	patch.SegmentIndex = req.SegmentIndex.Value
	if req.AltText != nil {
		normalized, err := normalizeAltText(*req.AltText)
		if err != nil {
			return patch, err
		}
		patch.AltText = &normalized
	}
	return patch, nil
}

// Update godoc
// @Summary      Update a post attachment
// @Description  Updates an attachment's `position` and/or `alt_text` (at least
// @Description  one required). Last-write-wins; no optimistic concurrency token
// @Description  (CON-73 §6 MVP decision).
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string                   true  "Post Sqid"
// @Param        id       path      string                   true  "Attachment id"
// @Param        body     body      updateAttachmentRequest  true  "Fields to update"
// @Success      200      {object}  attachmentResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/{id} [patch]
func (h *PostAttachmentsHandler) Update(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if err := ensureMutable(post); err != nil {
		return err
	}

	att, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "attachment not found")
	}
	if att.PostID != post.ID {
		return fiber.NewError(fiber.StatusNotFound, "attachment not found")
	}

	var req updateAttachmentRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	patch, err := req.toPatch(post)
	if err != nil {
		return err
	}

	if err := h.repo.Patch(reqCtx(c), att.ID, patch); err != nil {
		// UNIQUE(post_id, position): another attachment already holds the target
		// position. Point the caller at the atomic reorder endpoint instead.
		if isUniqueViolationOn(err, postAttachmentsPositionConstraint) {
			return fiber.NewError(fiber.StatusConflict,
				"another attachment already holds that position; PATCH /attachments/reorder to reorder the whole list atomically")
		}
		return err
	}

	updated, err := h.repo.GetByID(reqCtx(c), att.ID)
	if err != nil {
		return err
	}
	h.hydratePresigned(c, updated)
	return c.JSON(attachmentResponse{
		PostAttachment:     updated,
		PlatformValidation: platforms.ValidateAttachment(updated, post.Platform),
	})
}

type reorderAllRequest struct {
	// IDs is the post's attachments in their new order. It must list every
	// current attachment exactly once.
	IDs []string `json:"ids"`
}

// ReorderAll godoc
// @Summary      Reorder all post attachments (atomic)
// @Description  Renumbers the post's attachments to match `ids` (0..n-1) in one
// @Description  transaction, so the whole list reorders in a single request
// @Description  without tripping UNIQUE(post_id, position) (CON-124). `ids` must
// @Description  list every current attachment exactly once. Returns the
// @Description  reordered list.
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string             true  "Post Sqid"
// @Param        body     body      reorderAllRequest  true  "Attachment ids in the new order"
// @Success      200      {object}  listResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/reorder [patch]
func (h *PostAttachmentsHandler) ReorderAll(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if err := ensureMutable(post); err != nil {
		return err
	}

	var req reorderAllRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if len(req.IDs) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "ids is required")
	}

	current, err := h.repo.ListByPostID(reqCtx(c), post.ID)
	if err != nil {
		return err
	}
	// The ordered ids must be exactly the post's current attachments — same
	// count, same members, no duplicates — so the renumber covers every row and
	// none is left in the temporary offset range.
	if len(req.IDs) != len(current) {
		return fiber.NewError(fiber.StatusBadRequest, "ids must list every attachment of the post exactly once")
	}
	valid := make(map[string]bool, len(current))
	for i := range current {
		valid[current[i].ID] = true
	}
	seen := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		if !valid[id] || seen[id] {
			return fiber.NewError(fiber.StatusBadRequest, "ids must list every attachment of the post exactly once")
		}
		seen[id] = true
	}

	if err := h.repo.ReorderPositions(reqCtx(c), post.ID, req.IDs); err != nil {
		return err
	}

	updated, err := h.repo.ListByPostID(reqCtx(c), post.ID)
	if err != nil {
		return err
	}
	out := listResponse{
		Attachments:        make([]attachmentResponse, 0, len(updated)),
		PlatformValidation: platforms.ValidatePostAttachments(updated, post.Platform),
	}
	for i := range updated {
		h.hydratePresigned(c, &updated[i])
		out.Attachments = append(out.Attachments, attachmentResponse{
			PostAttachment:     &updated[i],
			PlatformValidation: platforms.ValidateAttachment(&updated[i], post.Platform),
		})
	}
	return c.JSON(out)
}

// Delete godoc
// @Summary      Delete a post attachment
// @Description  Removes an attachment row and its S3 object(s). For PDF
// @Description  attachments this includes the rendered thumbnail. If any
// @Description  S3 delete fails, the metadata row is retained and 502 is
// @Description  returned so the caller can retry (CON-73 §2.7).
// @Tags         post-attachments
// @Security     CookieAuth
// @Param        post_id  path  string  true  "Post Sqid"
// @Param        id       path  string  true  "Attachment id"
// @Success      204
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      502      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/{id} [delete]
func (h *PostAttachmentsHandler) Delete(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if err := ensureMutable(post); err != nil {
		return err
	}

	att, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "attachment not found")
	}
	if att.PostID != post.ID {
		return fiber.NewError(fiber.StatusNotFound, "attachment not found")
	}

	// Delete S3 first so we never end up with a row pointing at a
	// missing object. If S3 delete fails, the row stays and the caller
	// retries.
	if h.storage != nil {
		if att.S3Key != "" {
			if err := h.storage.Delete(reqCtx(c), att.S3Key); err != nil {
				return fiber.NewError(fiber.StatusBadGateway, "failed to delete object from storage; please retry")
			}
		}
		if att.ThumbnailS3Key != "" {
			if err := h.storage.Delete(reqCtx(c), att.ThumbnailS3Key); err != nil {
				return fiber.NewError(fiber.StatusBadGateway, "failed to delete thumbnail from storage; please retry")
			}
		}
	}

	deleted, err := h.repo.Delete(reqCtx(c), att.ID)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "attachment not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
