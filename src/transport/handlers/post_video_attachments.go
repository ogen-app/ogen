package handlers

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/grpc/client/video"
)

// presignPutTTL bounds how long a video-upload PUT URL stays valid — long
// enough for a large upload over a slow link, short enough that a leaked URL
// expires quickly.
const presignPutTTL = 30 * time.Minute

// probeGetTTL is the lifetime of the presigned GET URL handed to
// video-service so it can range-read the uploaded object. It only needs to
// outlive a single Probe call.
const probeGetTTL = 5 * time.Minute

// VideoProber probes an uploaded video for duration/codec/resolution and a
// poster frame via video-service (CON-148). Implemented by *video.Client;
// an interface here keeps the handler testable and nil-tolerant (nil disables
// probing — uploads are accepted unprobed).
type VideoProber interface {
	Probe(ctx context.Context, opts video.ProbeOptions) (*video.ProbeResult, error)
}

// presignVideoRequest is the presign body: the client declares what it is
// about to upload so the server can cap size and pick a storage key.
type presignVideoRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type presignVideoResponse struct {
	UploadURL string `json:"upload_url"`
	S3Key     string `json:"s3_key"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// PresignVideo godoc
// @Summary      Presign a direct-to-storage video upload
// @Description  Returns a short-lived PUT URL the client uses to upload video
// @Description  bytes straight to object storage, bypassing the API process so
// @Description  multi-GB files never buffer in memory (CON-148). The client
// @Description  then calls finalize with the returned `s3_key`. Only video
// @Description  content types are accepted here; images/PDFs use the direct
// @Description  upload endpoint. Hard cap: 5 GiB.
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string               true  "Post Sqid"
// @Param        body     body      presignVideoRequest  true  "Declared upload"
// @Success      200      {object}  presignVideoResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      415      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/presign [post]
func (h *PostAttachmentsHandler) PresignVideo(c *fiber.Ctx) error {
	if h.storage == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "storage not configured")
	}
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	if lockedForMutations(post.Status) {
		return fiber.NewError(fiber.StatusConflict, "post has been submitted (scheduled or published) and its attachments are locked")
	}

	var req presignVideoRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	req.ContentType = strings.TrimSpace(strings.ToLower(req.ContentType))
	if !strings.HasPrefix(req.ContentType, "video/") {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "presign accepts video content types only; use the direct upload endpoint for images and PDFs")
	}
	if req.SizeBytes <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "size_bytes is required and must be positive")
	}
	if req.SizeBytes > maxVideoUploadBytes() {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("video exceeds upload limit of %d GB", maxVideoUploadBytes()>>30))
	}

	// A random token, not the eventual row id, gives the key uniqueness — the
	// row id is minted at finalize. The key is namespaced by tenant + post so
	// finalize can prove ownership by prefix.
	token, err := models.NewID()
	if err != nil {
		return err
	}
	key := storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/"+token+videoContentTypeToExt(req.ContentType))

	url, err := h.storage.PresignedPutURL(c.Context(), key, req.ContentType, presignPutTTL)
	if err != nil {
		return fmt.Errorf("post_attachments: presign put: %w", err)
	}
	return c.JSON(presignVideoResponse{
		UploadURL: url,
		S3Key:     key,
		ExpiresIn: int(presignPutTTL / time.Second),
	})
}

// finalizeVideoRequest finalizes a previously-presigned upload.
type finalizeVideoRequest struct {
	S3Key   string `json:"s3_key"`
	AltText string `json:"alt_text"`
}

// FinalizeVideo godoc
// @Summary      Finalize a presigned video upload
// @Description  Probes the uploaded object via video-service (duration, codec,
// @Description  resolution, poster frame), validates it against the post's
// @Description  platform, and persists the attachment row (CON-148). Corrupt
// @Description  or unreadable video is a terminal 400; an unrecognised
// @Description  container/codec is 415. If video-service is unreachable the
// @Description  attachment is still created, unprobed (no duration/poster,
// @Description  weaker validation).
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string                true  "Post Sqid"
// @Param        body     body      finalizeVideoRequest  true  "Finalize body"
// @Success      201      {object}  attachmentResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      415      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/finalize [post]
func (h *PostAttachmentsHandler) FinalizeVideo(c *fiber.Ctx) error {
	if h.storage == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "storage not configured")
	}
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	if lockedForMutations(post.Status) {
		return fiber.NewError(fiber.StatusConflict, "post has been submitted (scheduled or published) and its attachments are locked")
	}

	var req finalizeVideoRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.S3Key == "" {
		return fiber.NewError(fiber.StatusBadRequest, "s3_key is required")
	}
	// Ownership: the key must sit under this tenant+post prefix, exactly as
	// PresignVideo minted it. This blocks finalizing an arbitrary object (or
	// another post's / tenant's upload).
	wantPrefix := storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/")
	if !strings.HasPrefix(req.S3Key, wantPrefix) {
		return fiber.NewError(fiber.StatusBadRequest, "s3_key does not belong to this post")
	}

	altText, err := normalizeAltText(req.AltText)
	if err != nil {
		return err
	}

	// Authoritative size comes from the stored object, not the client.
	info, err := h.storage.Head(c.Context(), req.S3Key)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "uploaded object not found; PUT the bytes to the presigned URL first")
	}
	if info.Size == 0 {
		_ = h.storage.Delete(c.Context(), req.S3Key)
		return fiber.NewError(fiber.StatusBadRequest, "uploaded object is empty")
	}
	if info.Size > maxVideoUploadBytes() {
		_ = h.storage.Delete(c.Context(), req.S3Key)
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("video exceeds upload limit of %d GB", maxVideoUploadBytes()>>30))
	}

	session := c.Locals("session").(*models.Session)
	id, err := models.NewID()
	if err != nil {
		return err
	}
	att := &models.PostAttachment{
		ID:        id,
		PostID:    post.ID,
		AltText:   altText,
		SizeBytes: info.Size,
		S3Key:     req.S3Key,
		CreatedBy: session.UserID,
	}

	// Probe via video-service (CON-148). Best-effort like pdf-service: a
	// terminal "not a readable video" verdict rejects the upload; transient /
	// unreachable failures degrade to an unprobed attachment.
	var probe *video.ProbeResult
	if h.video != nil {
		srcURL, perr := h.storage.PresignedGetURL(c.Context(), req.S3Key, probeGetTTL)
		if perr != nil {
			slog.WarnContext(c.Context(), "video probe presign failed", logging.AttrComponent, "post_attachments", logging.AttrError, perr)
		} else {
			pctx, cancel := context.WithTimeout(c.Context(), videoProbeTimeout)
			res, rerr := h.video.Probe(pctx, video.ProbeOptions{
				SourceURL:    srcURL,
				RenderPoster: true,
				Filename:     path.Base(req.S3Key),
			})
			cancel()
			if rerr != nil {
				if video.IsInvalidVideo(rerr) {
					_ = h.storage.Delete(c.Context(), req.S3Key)
					return fiber.NewError(fiber.StatusBadRequest, "uploaded file is not a readable video")
				}
				slog.WarnContext(c.Context(), "video probe failed", logging.AttrComponent, "post_attachments", "key", req.S3Key, logging.AttrError, rerr)
			} else {
				probe = res
				att.DurationMs = res.DurationMs
				att.Codec = res.Codec
				att.Width = res.Width
				att.Height = res.Height
			}
		}
	}

	// Resolve the MIME type so kind detection routes this to the video
	// validator. Prefer the probed container; fall back to the stored
	// content-type, then the key extension. If none yields a video type the
	// container is unsupported.
	att.MimeType = resolveVideoMIME(probe, info.ContentType, req.S3Key)
	if !strings.HasPrefix(att.MimeType, "video/") {
		_ = h.storage.Delete(c.Context(), req.S3Key)
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported video container or codec")
	}

	// Store the poster frame as the attachment thumbnail before the insert so
	// ThumbnailS3Key is persisted in one write (mirrors the PDF path). Storing
	// it is best-effort — the attachment stays valid without a thumbnail.
	if probe != nil && len(probe.PosterPNG) > 0 {
		thumbKey := storage.TenantKey(c.Context(), "post-attachments/"+post.ID+"/"+id+".thumb.png")
		if _, uerr := h.storage.Upload(c.Context(), thumbKey, bytes.NewReader(probe.PosterPNG), int64(len(probe.PosterPNG)), "image/png"); uerr == nil {
			att.ThumbnailS3Key = thumbKey
		}
	}

	if err := h.repo.CreateAtNextPosition(c.Context(), att); err != nil {
		// Clean up the orphan objects — without the metadata row the bytes are
		// dead weight in the bucket.
		_ = h.storage.Delete(c.Context(), att.S3Key)
		if att.ThumbnailS3Key != "" {
			_ = h.storage.Delete(c.Context(), att.ThumbnailS3Key)
		}
		return err
	}

	h.hydratePresigned(c, att)
	return c.Status(fiber.StatusCreated).JSON(attachmentResponse{
		PostAttachment:     att,
		PlatformValidation: platforms.ValidateAttachment(att, post.Platform),
	})
}

// resolveVideoMIME picks the canonical video MIME for a finalized upload.
func resolveVideoMIME(probe *video.ProbeResult, storedContentType, key string) string {
	if probe != nil {
		if m := containerToVideoMIME(probe.Container, path.Ext(key)); m != "" {
			return m
		}
	}
	if strings.HasPrefix(storedContentType, "video/") {
		return storedContentType
	}
	return extToVideoMIME(path.Ext(key))
}

// videoContentTypeToExt maps a declared video MIME to a storage-key extension.
// Unknown video types get ".bin"; the object still stores fine and
// video-service remains the structural validator.
func videoContentTypeToExt(ct string) string {
	switch ct {
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "video/x-msvideo":
		return ".avi"
	case "video/x-matroska":
		return ".mkv"
	case "video/mpeg":
		return ".mpeg"
	case "video/3gpp":
		return ".3gp"
	}
	return ".bin"
}

// extToVideoMIME is the reverse of videoContentTypeToExt — the fallback when
// neither the probe nor the stored content-type identifies the container.
func extToVideoMIME(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mkv":
		return "video/x-matroska"
	case ".mpeg", ".mpg":
		return "video/mpeg"
	case ".3gp":
		return "video/3gpp"
	}
	return ""
}

// containerToVideoMIME maps a video-service ffprobe container name (which can
// be a comma list, e.g. "mov,mp4,m4a,3gp,3g2,mj2") to a canonical video MIME.
// Matroska and WebM share a demuxer, so ffprobe reports both as
// "matroska,webm"; ext (the attachment's file extension) disambiguates that
// case — ".mkv" is Matroska, otherwise WebM.
func containerToVideoMIME(container, ext string) string {
	c := strings.ToLower(strings.TrimSpace(container))
	ext = strings.ToLower(ext)
	hasMatroska := strings.Contains(c, "matroska") || c == "mkv"
	hasWebM := strings.Contains(c, "webm")
	switch {
	case c == "":
		return ""
	case strings.Contains(c, "mp4") || c == "m4v" || c == "mov" || c == "quicktime":
		return "video/mp4"
	case hasMatroska && hasWebM:
		// Ambiguous shared demuxer — decide by extension, defaulting to WebM.
		if ext == ".mkv" {
			return "video/x-matroska"
		}
		return "video/webm"
	case hasWebM:
		return "video/webm"
	case hasMatroska:
		return "video/x-matroska"
	case c == "avi":
		return "video/x-msvideo"
	case strings.HasPrefix(c, "mpeg") || c == "mpegts" || c == "mpegvideo":
		return "video/mpeg"
	case strings.HasPrefix(c, "3gp"):
		return "video/3gpp"
	}
	return ""
}
