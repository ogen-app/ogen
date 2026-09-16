package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/entitlements"
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
)

// The upload ceilings (image/pdf/video bytes, alt-text length) are no longer
// constants: CON-292 moved them into operator-controlled global config. They
// are read through the accessors in global_limits.go.
const (
	// postAttachmentsPositionConstraint is the name Postgres gives the inline
	// UNIQUE (post_id, position) on post_attachments (baseline schema). Used to
	// scope the reorder 409 to that specific collision (CON-124).
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
// via pdf-service (CON-103). Implemented by *pdf.Client; an interface here
// keeps the handler testable and nil-tolerant (nil disables it).
type PDFRenderer interface {
	Render(ctx context.Context, r io.Reader, opts pdf.RenderOptions) (*pdf.RenderResult, error)
}

// ImagePreparer runs the image-service light path (CON-281): validate + metadata
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
// for post attachments — images (CON-73) and PDFs (CON-75). All
// mutations are blocked once the parent post is submitted — scheduled or
// published (CON-251); see lockedForMutations.
type PostAttachmentsHandler struct {
	repo     repository.PostAttachmentRepository
	postRepo repository.PostRepository
	storage  storage.Storage
	pdf      PDFRenderer
	video    VideoProber
	// image runs the CON-281 light path (EXIF-strip + metadata + async alt text).
	// Nil disables image attachments (image-service unwired, D6). recorder meters
	// the alt-text vision call (CON-86, nil-safe); altTextModel/altTextMaxChars are
	// the generation model + target length.
	image           ImagePreparer
	recorder        *usage.Recorder
	altTextModel    string
	altTextMaxChars int
	auth            fiber.Handler
	limiter         *entitlements.Limiter // CON-295 media_storage_bytes quota (nil-safe)
}

// SetLimiter wires the CON-295 entitlement limiter (nil-safe no-op).
func (h *PostAttachmentsHandler) SetLimiter(l *entitlements.Limiter) { h.limiter = l }

func NewPostAttachmentsHandler(
	repo repository.PostAttachmentRepository,
	postRepo repository.PostRepository,
	store storage.Storage,
	renderer PDFRenderer,
	prober VideoProber,
	preparer ImagePreparer,
	recorder *usage.Recorder,
	altTextModel string,
	altTextMaxChars int,
	auth fiber.Handler,
) *PostAttachmentsHandler {
	return &PostAttachmentsHandler{
		repo:            repo,
		postRepo:        postRepo,
		storage:         store,
		pdf:             renderer,
		video:           prober,
		image:           preparer,
		recorder:        recorder,
		altTextModel:    altTextModel,
		altTextMaxChars: altTextMaxChars,
		auth:            auth,
	}
}

func (h *PostAttachmentsHandler) Register(app *fiber.App) {
	g := app.Group("/api/posts/:post_id/attachments", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Upload)
	// Large-file video ingest (CON-148): presign a direct-to-S3 PUT, then
	// finalize (probe + validate + persist). Static paths are registered
	// before the /:id param route so they aren't captured as id="presign".
	g.Post("/presign", h.PresignVideo)
	g.Post("/finalize", h.FinalizeVideo)
	// Static /reorder is registered before the /:id param route so a PATCH to
	// .../attachments/reorder isn't captured as id="reorder" (CON-124).
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

// loadPostOrErr fetches the post and returns 404 if missing. Mutating
// callers should additionally check lockedForMutations.
func (h *PostAttachmentsHandler) loadPostOrErr(c *fiber.Ctx) (*models.Post, error) {
	postID := c.Params("post_id")
	post, err := h.postRepo.GetByID(c.Context(), postID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		return nil, err
	}
	return post, nil
}

// lockedForMutations reports whether the post is in a state that freezes
// its attachments. Originally published-only (CON-73 §2.1 / §2.7); CON-251
// generalises it to every submitted state via IsSubmitted, so a scheduled
// post's media freezes too — Zernio snapshots the attachments at schedule
// time, so a later change here would silently diverge from what publishes.
func lockedForMutations(s models.PostStatus) bool {
	return s.IsSubmitted()
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
		if url, err := h.storage.PresignedGetURL(c.Context(), att.S3Key, PresignedURLTTL); err == nil {
			att.PresignedURL = url
		}
	}
	if att.ThumbnailS3Key != "" {
		if url, err := h.storage.PresignedGetURL(c.Context(), att.ThumbnailS3Key, PresignedURLTTL); err == nil {
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
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	atts, err := h.repo.ListByPostID(c.Context(), post.ID)
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
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	att, err := h.repo.GetByID(c.Context(), c.Params("id"))
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
// stable, machine-readable code beside the human message (CON-281). It mirrors
// the batch content-bank upload's {code,error} shape so the front-end can match
// on the code across both surfaces and fall back to the prose when it is
// unknown. It returns nil because the response is already written — the central
// error handler has no slot for a code, so it is intentionally bypassed (4xx
// client rejections are not error-logged there anyway).
func rejectAttachment(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(fiber.Map{"code": code, "error": msg})
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
// @Description  multipart/form-data under the field `file`. The file is
// @Description  decoded server-side to validate the MIME — JPEG/PNG/WebP/GIF
// @Description  for images, application/pdf for PDFs — then streamed to
// @Description  object storage. Hard caps: 50 MB images, 100 MB PDFs.
// @Description  PDF uploads also render a first-page PNG thumbnail
// @Description  (best-effort; failures do not abort the upload).
// @Description  Per-platform caps are surfaced as soft warnings in the
// @Description  response.
// @Tags         post-attachments
// @Accept       multipart/form-data
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string  true  "Post Sqid"
// @Param        file     formData  file    true  "Image or PDF file"
// @Success      201      {object}  attachmentResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string  "post is in a terminal publishing state"
// @Failure      415      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments [post]
func (h *PostAttachmentsHandler) Upload(c *fiber.Ctx) error {
	if h.storage == nil {
		return rejectAttachment(c, fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "storage not configured")
	}

	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	if lockedForMutations(post.Status) {
		return fiber.NewError(fiber.StatusConflict, "post has been submitted (scheduled or published) and its attachments are locked")
	}

	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file is required")
	}
	// Reject anything larger than the largest per-kind cap before we even open
	// the file — this handler accepts both images and PDFs, and the image cap can
	// exceed the PDF cap (both are operator-configurable), so gate on the larger
	// of the two. The precise per-kind cap is enforced again inside the probe.
	preSniffCap := maxPDFUploadBytes()
	if img := maxImageUploadBytes(); img > preSniffCap {
		preSniffCap = img
	}
	if fh.Size > preSniffCap {
		return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeTooLarge,
			fmt.Sprintf("file exceeds upload limit of %d MB", preSniffCap>>20))
	}
	// CON-295: this upload adds fh.Size bytes to the tenant's media_storage_bytes
	// budget. Check before touching storage so a denied upload never leaves an
	// orphaned object behind.
	var mediaQuota entitlements.Decision
	tenantID, hasTenant := tenantctx.From(c.Context())
	if hasTenant {
		dec, qErr := h.limiter.RequireAmount(c.Context(), tenantID, "media_storage_bytes", fh.Size)
		if qErr != nil {
			return qErr
		}
		mediaQuota = dec
	}

	f, err := fh.Open()
	if err != nil {
		return fmt.Errorf("post_attachments: open upload: %w", err)
	}
	defer f.Close()

	// Sniff magic bytes to decide image vs PDF without trusting the
	// client's declared Content-Type or filename.
	sniff := make([]byte, 512)
	n, _ := io.ReadFull(f, sniff)
	if n == 0 {
		return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeEmptyFile, "file is empty")
	}
	kind := http.DetectContentType(sniff[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("post_attachments: rewind upload: %w", err)
	}

	session := c.Locals("session").(*models.Session)
	id, err := models.NewID()
	if err != nil {
		return err
	}

	altText, err := normalizeAltText(c.FormValue("alt_text"))
	if err != nil {
		return err
	}

	// CON-284: on a thread post the client names which segment this media
	// belongs to (0-based). Omitted/blank ⇒ NULL (whole-post attachment,
	// today's behaviour). Range against thread_segments is enforced at the
	// publish gate, not here — media may be uploaded before segments are set.
	segIdx, err := parseSegmentIndex(c.FormValue("segment_index"))
	if err != nil {
		return err
	}
	// A segment only exists on a thread post; refuse to stamp one on an ordinary
	// post so a meaningless index is never stored or returned (CON-284 §9).
	if segIdx != nil && !post.IsThread() {
		return fiber.NewError(fiber.StatusUnprocessableEntity, "segment_index is only valid on a thread post")
	}

	att := &models.PostAttachment{
		ID:           id,
		PostID:       post.ID,
		AltText:      altText,
		SegmentIndex: segIdx,
		CreatedBy:    session.UserID,
		// A user-supplied alt text on upload is a manual edit (CON-281 D5): mark it
		// so the async auto-generator never overwrites it.
		AltTextEditedByUser: altText != "",
	}
	var data []byte
	var keyExt string
	var pendingThumbnail []byte
	// imagePrepared marks that the image branch already wrote the (EXIF-stripped)
	// object to att.S3Key via image-service, so the shared upload tail is skipped.
	var imagePrepared bool

	if kind == pdfprobe.MIME {
		if fh.Size > maxPDFUploadBytes() {
			return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeTooLarge,
				fmt.Sprintf("PDF exceeds upload limit of %d MB", maxPDFUploadBytes()>>20))
		}
		probe, raw, err := pdfprobe.Probe(f, maxPDFUploadBytes())
		if err != nil {
			if errors.Is(err, pdfprobe.ErrUnsupportedMIME) {
				return rejectAttachment(c, fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, err.Error())
			}
			return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeInvalidFile, err.Error())
		}
		att.MimeType = probe.MIME
		att.SizeBytes = probe.Size
		att.ChecksumSHA256 = probe.SHA256
		data = raw
		keyExt = probe.Extension

		// Page count + first-page thumbnail come from pdf-service (CON-103).
		// Best-effort: a render failure leaves page_count 0 / no thumbnail
		// rather than failing the upload. page_count is set here, before the
		// platform soft-validation below, so the max_pages check still works.
		if h.pdf != nil {
			rctx, cancel := context.WithTimeout(c.Context(), pdfRenderTimeout)
			render, rerr := h.pdf.Render(rctx, bytes.NewReader(raw), pdf.RenderOptions{
				RenderThumbnail: true,
				ThumbnailDPI:    pdfThumbnailDPI,
			})
			cancel()
			if rerr != nil {
				// A terminal "not a usable PDF" verdict (corrupt/encrypted/not a
				// PDF) is a client error — reject before storing anything. Magic
				// bytes alone are not enough; pdfprobe only checks the prefix, so
				// pdf-service is the structural validator. Transient/unreachable
				// failures degrade gracefully: keep the attachment, no page count
				// or thumbnail.
				if pdf.IsInvalidPDF(rerr) {
					return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeInvalidFile, "uploaded file is not a readable PDF")
				}
				slog.WarnContext(c.Context(), "pdf render failed", logging.AttrComponent, "post_attachments", "name", fh.Filename, logging.AttrError, rerr)
			} else {
				att.PageCount = render.PageCount
				pendingThumbnail = render.ThumbnailPNG
			}
		}
	} else {
		if fh.Size > maxImageUploadBytes() {
			return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeTooLarge,
				fmt.Sprintf("image exceeds upload limit of %d MB", maxImageUploadBytes()>>20))
		}
		// image-service is the sole image authority (D6): validate + metadata +
		// EXIF-strip (pixels preserved) all happen there. Nil client → reject
		// (no imageprobe fallback).
		if h.image == nil {
			return rejectAttachment(c, fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "image processing is not configured")
		}
		// The extension routes the object key + presign content-type (the service
		// preserves the format, pixels intact); the body is still sniffed
		// authoritatively by image-service. SVG/unknown → 415.
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		mime, ok := imageUploadMIMEs[ext]
		if !ok {
			// An SVG lands here (it's in no raster allowlist); name it specifically.
			if ext == ".svg" {
				return rejectAttachment(c, fiber.StatusUnsupportedMediaType, models.UploadCodeVectorRejected, "SVG / vector images are not supported — upload a raster image (JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, or BMP)")
			}
			return rejectAttachment(c, fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "unsupported image type — accepted: JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, BMP")
		}
		raw, rerr := io.ReadAll(f)
		if rerr != nil {
			return fmt.Errorf("post_attachments: read image: %w", rerr)
		}

		// Stage the EXIF-bearing original at a temp key, hand image-service presigned
		// GET/PUT, and let it write the cleaned (metadata-stripped, pixel-identical)
		// copy to the final key. The original is discarded after — its EXIF /
		// geolocation must never persist or publish (CON-281 §7, §17).
		cleanKey := storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/"+id+ext)
		origKey := storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/"+id+".orig"+ext)
		if _, err := h.storage.Upload(c.Context(), origKey, bytes.NewReader(raw), int64(len(raw)), mime); err != nil {
			return fmt.Errorf("post_attachments: stage original: %w", err)
		}
		getURL, gerr := h.storage.PresignedGetURL(c.Context(), origKey, PresignedURLTTL)
		putURL, perr := h.storage.PresignedPutURL(c.Context(), cleanKey, mime, PresignedURLTTL)
		if gerr != nil || perr != nil {
			_ = h.storage.Delete(c.Context(), origKey)
			return fmt.Errorf("post_attachments: presign image transfer: get=%v put=%v", gerr, perr)
		}
		prep, err := h.image.PrepareAttachment(c.Context(), imageclient.PrepareAttachmentOptions{
			SourceURL:     getURL,
			DestPutURL:    putURL,
			StripMetadata: true,
			WantAltText:   false, // alt text is generated asynchronously below
			Filename:      fh.Filename,
		})
		if err != nil {
			// Drop BOTH the staged original and any partial cleaned object the service
			// may have written to cleanKey before failing — never leave orphaned bytes.
			_ = h.storage.Delete(c.Context(), origKey)
			_ = h.storage.Delete(c.Context(), cleanKey)
			// Prefer the fine-grained reason image-service attaches to a terminal
			// reject over the coarse gRPC-code buckets (CON-281 Phase 2); the buckets
			// stay as the fallback for an older service that carries no ErrorInfo.
			if code := imageclient.UploadCode(err); code != "" {
				return rejectAttachment(c, imageRejectStatus(code), code, models.UploadRejectMessage(code))
			}
			switch {
			case imageclient.IsUnsupportedImage(err):
				return rejectAttachment(c, fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "unsupported image format")
			case imageclient.IsInvalidImage(err):
				return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeInvalidFile, "uploaded file is not a readable image")
			default:
				// Transient / unreachable — no imageprobe fallback (D6).
				return rejectAttachment(c, fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "image processing is temporarily unavailable; please retry")
			}
		}
		if prep.RejectedReason != "" {
			_ = h.storage.Delete(c.Context(), origKey)
			_ = h.storage.Delete(c.Context(), cleanKey)
			// A structured verdict from the service, carried through with its own
			// message. Phase 1 maps it to the coarse "not a usable image" bucket; the
			// image.v1 RejectedCode enum (CON-281 Phase 2) refines it to the exact
			// reason (vector / oversize / corrupt) without a client change.
			return rejectAttachment(c, fiber.StatusBadRequest, models.UploadCodeInvalidFile, prep.RejectedReason)
		}
		att.MimeType = prep.Mime
		att.SizeBytes = prep.SizeBytes
		att.Width = prep.Width
		att.Height = prep.Height
		att.IsAnimated = prep.IsAnimated
		att.ChecksumSHA256 = prep.ChecksumSHA256
		att.S3Key = cleanKey
		imagePrepared = true
		// The EXIF-bearing original has served its purpose — drop it (best-effort).
		_ = h.storage.Delete(c.Context(), origKey)
	}

	if !imagePrepared {
		att.S3Key = storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/"+id+keyExt)
		if _, err := h.storage.Upload(c.Context(), att.S3Key, bytes.NewReader(data), att.SizeBytes, att.MimeType); err != nil {
			return fmt.Errorf("post_attachments: storage upload: %w", err)
		}
	}

	// Upload the first-page thumbnail rendered by pdf-service (if any). Storing
	// it is best-effort — the attachment stays valid without a thumbnail.
	if len(pendingThumbnail) > 0 {
		thumbKey := storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/"+id+".thumb.png")
		if _, uerr := h.storage.Upload(c.Context(), thumbKey, bytes.NewReader(pendingThumbnail), int64(len(pendingThumbnail)), "image/png"); uerr == nil {
			att.ThumbnailS3Key = thumbKey
		}
	}

	// CreateAtNextPosition assigns att.Position atomically, eliminating
	// the read-then-insert race that NextPosition+Create exposed under
	// concurrent uploads to the same post.
	if err := h.repo.CreateAtNextPosition(c.Context(), att); err != nil {
		// Clean up the orphan objects — the metadata row is what makes
		// the attachment discoverable; without it, the bytes are dead
		// weight in the bucket (CON-73 §2.2 transactional rollback).
		_ = h.storage.Delete(c.Context(), att.S3Key)
		if att.ThumbnailS3Key != "" {
			_ = h.storage.Delete(c.Context(), att.ThumbnailS3Key)
		}
		return err
	}
	// CON-295: the attachment (and its bytes) now exist — fire any near-limit crossing.
	if hasTenant {
		h.limiter.DispatchCrossing(c.Context(), tenantID, mediaQuota)
	}

	// Auto-generate alt text asynchronously for an image with no user-supplied one
	// (CON-281 §10): the synchronous upload stays fast, and the generator writes
	// only where alt text is still un-edited.
	if imagePrepared && att.AltText == "" && h.image != nil {
		go h.generateAttachmentAltText(context.WithoutCancel(c.Context()), session.TenantID, att.ID, att.S3Key)
	}

	h.hydratePresigned(c, att)
	return c.Status(fiber.StatusCreated).JSON(attachmentResponse{
		PostAttachment:     att,
		PlatformValidation: platforms.ValidateAttachment(att, post.Platform),
	})
}

// generateAttachmentAltText runs image-service's GenerateAltText for a freshly
// uploaded image attachment and stores the result (CON-281 §10). Fire-and-forget:
// it runs in its own goroutine off a detached context, meters the vision call
// (CON-86), and persists only where the user hasn't edited the alt text (D5).
// Best-effort throughout — a failure just leaves the attachment without alt text
// (regeneration is a separate, explicit action).
func (h *PostAttachmentsHandler) generateAttachmentAltText(ctx context.Context, tenantID, attID, s3Key string) {
	if h.image == nil || h.storage == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Rebuild the tenant context the request carried, so the presign + repo write
	// run against the right tenant (CON-97).
	ctx = tenantctx.With(ctx, tenantID)

	getURL, err := h.storage.PresignedGetURL(ctx, s3Key, PresignedURLTTL)
	if err != nil {
		return
	}
	res, err := h.image.GenerateAltText(ctx, imageclient.GenerateAltTextOptions{
		SourceURL: getURL,
		MaxChars:  h.altTextMaxChars,
		Model:     h.altTextModel,
	})
	if err != nil || res == nil {
		slog.WarnContext(ctx, "attachment alt-text generation failed", logging.AttrComponent, "post_attachments", "attachment_id", attID, logging.AttrError, err)
		return
	}
	// Meter the vision call on the gemini vendor (CON-86); RecordResp is nil-safe.
	for _, u := range res.Usage {
		h.recorder.RecordResp(ctx, llm.VendorGemini, u.Model, "alt_text", llm.VisionUsage{Step: u.Step, InputTokens: u.Input, OutputTokens: u.Output})
	}
	alt := strings.TrimSpace(res.AltText)
	if alt == "" {
		return
	}
	// Guard the storage cap (runes) — the generation target is short, but stay safe.
	if utf8.RuneCountInString(alt) > maxAltTextLen() {
		alt = string([]rune(alt)[:maxAltTextLen()])
	}
	if err := h.repo.SetGeneratedAltText(ctx, attID, alt); err != nil {
		slog.WarnContext(ctx, "attachment alt-text persist failed", logging.AttrComponent, "post_attachments", "attachment_id", attID, logging.AttrError, err)
	}
}

// parseSegmentIndex parses the optional segment_index form/JSON value for a
// thread attachment (CON-284). A blank value yields nil (NULL — a whole-post
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

// normalizeAltText trims and length-bounds accessibility alt text (CON-122).
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
// one must be present. position reorders (CON-73); alt_text sets accessibility
// text (CON-122).
type updateAttachmentRequest struct {
	Position *int    `json:"position"`
	AltText  *string `json:"alt_text"`
	// SegmentIndex reassigns which thread segment the media belongs to (CON-284).
	// Presence-aware so the three JSON states stay distinct: absent ⇒ leave as-is,
	// a number ⇒ move to that segment, explicit null ⇒ detach (back to a
	// whole-post attachment).
	SegmentIndex Optional[int] `json:"segment_index"`
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
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	if lockedForMutations(post.Status) {
		return fiber.NewError(fiber.StatusConflict, "post has been submitted (scheduled or published) and its attachments are locked")
	}

	att, err := h.repo.GetByID(c.Context(), c.Params("id"))
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
	if req.Position == nil && req.AltText == nil && !req.SegmentIndex.Present {
		return fiber.NewError(fiber.StatusBadRequest, "at least one of position, alt_text or segment_index is required")
	}

	// Validate + normalize all inputs before any mutation, so an invalid
	// field can't leave a partially-applied update (e.g. position already
	// changed).
	if req.Position != nil && *req.Position < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "position must be non-negative")
	}
	if req.SegmentIndex.Present && req.SegmentIndex.Value != nil {
		if *req.SegmentIndex.Value < 0 {
			return fiber.NewError(fiber.StatusBadRequest, "segment_index must be non-negative")
		}
		// A non-null segment only belongs on a thread post; an explicit null is
		// always allowed (it clears the field back to a whole-post attachment).
		if !post.IsThread() {
			return fiber.NewError(fiber.StatusUnprocessableEntity, "segment_index is only valid on a thread post")
		}
	}
	var altText string
	if req.AltText != nil {
		normalized, err := normalizeAltText(*req.AltText)
		if err != nil {
			return err
		}
		altText = normalized
	}

	if req.Position != nil {
		if err := h.repo.UpdatePosition(c.Context(), att.ID, *req.Position); err != nil {
			// UNIQUE(post_id, position): another attachment already holds the
			// target position. Surface as 409 (not a raw 500) and point at the
			// atomic reorder endpoint (CON-124).
			if isUniqueViolationOn(err, postAttachmentsPositionConstraint) {
				return fiber.NewError(fiber.StatusConflict,
					"another attachment already holds that position; PATCH /attachments/reorder to reorder the whole list atomically")
			}
			return err
		}
	}
	if req.AltText != nil {
		if err := h.repo.UpdateAltText(c.Context(), att.ID, altText); err != nil {
			return err
		}
	}
	if req.SegmentIndex.Present {
		// Optional[int].Value is already the *int UpdateSegmentIndex wants: a
		// number sets the segment, an explicit null (nil) detaches it.
		if err := h.repo.UpdateSegmentIndex(c.Context(), att.ID, req.SegmentIndex.Value); err != nil {
			return err
		}
	}

	updated, err := h.repo.GetByID(c.Context(), att.ID)
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
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	if lockedForMutations(post.Status) {
		return fiber.NewError(fiber.StatusConflict, "post has been submitted (scheduled or published) and its attachments are locked")
	}

	var req reorderAllRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if len(req.IDs) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "ids is required")
	}

	current, err := h.repo.ListByPostID(c.Context(), post.ID)
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

	if err := h.repo.ReorderPositions(c.Context(), post.ID, req.IDs); err != nil {
		return err
	}

	updated, err := h.repo.ListByPostID(c.Context(), post.ID)
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
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	if lockedForMutations(post.Status) {
		return fiber.NewError(fiber.StatusConflict, "post has been submitted (scheduled or published) and its attachments are locked")
	}

	att, err := h.repo.GetByID(c.Context(), c.Params("id"))
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
			if err := h.storage.Delete(c.Context(), att.S3Key); err != nil {
				return fiber.NewError(fiber.StatusBadGateway, "failed to delete object from storage; please retry")
			}
		}
		if att.ThumbnailS3Key != "" {
			if err := h.storage.Delete(c.Context(), att.ThumbnailS3Key); err != nil {
				return fiber.NewError(fiber.StatusBadGateway, "failed to delete thumbnail from storage; please retry")
			}
		}
	}

	deleted, err := h.repo.Delete(c.Context(), att.ID)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "attachment not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
