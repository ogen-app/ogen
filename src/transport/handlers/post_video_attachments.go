package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
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
// poster frame via video-service. Implemented by *video.Client;
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
// @Description  upload endpoint. Hard cap: 5 GiB. The declared size is checked
// @Description  against the workspace's media storage quota (402 when over).
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string               true  "Post Sqid"
// @Param        body     body      presignVideoRequest  true  "Declared upload"
// @Success      200      {object}  presignVideoResponse
// @Failure      400      {object}  map[string]string "too_large, invalid request"
// @Failure      401      {object}  map[string]string
// @Failure      402      {object}  map[string]any    "media-storage limit reached"
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      415      {object}  map[string]string "unsupported_media_type"
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/presign [post]
func (h *PostAttachmentsHandler) PresignVideo(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	var req presignVideoRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	out, err := h.presignVideoUpload(c, post, req.ContentType, req.SizeBytes, maxVideoUploadBytes())
	if err != nil {
		return attachmentError(c, err)
	}
	return c.JSON(out)
}

// presignVideoUpload mints a presigned PUT for a declared video upload of at
// most maxBytes. The declared size is checked against media_storage_bytes up
// front so a workspace over its plan never uploads; finalize re-checks the
// real size. Upload rejects come back as *attachmentReject.
func (h *PostAttachmentsHandler) presignVideoUpload(c *fiber.Ctx, post *models.Post, contentType string, size, maxBytes int64) (*presignVideoResponse, error) {
	if h.storage == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "storage not configured")
	}
	if err := ensureMutable(post); err != nil {
		return nil, err
	}
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	if !strings.HasPrefix(contentType, "video/") {
		return nil, rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType,
			"presign accepts video content types only; use the direct upload endpoint for images and PDFs")
	}
	if size <= 0 {
		return nil, fiber.NewError(fiber.StatusBadRequest, "size_bytes is required and must be positive")
	}
	if size > maxBytes {
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeTooLarge, videoTooLargeMessage(maxBytes))
	}
	if _, err := requireQuotaAmount(c, h.limiter, "media_storage_bytes", size); err != nil {
		return nil, err
	}

	// A random token, not the eventual row id, gives the key uniqueness — the
	// row id is minted at finalize. The key is namespaced by tenant + post so
	// finalize can prove ownership by prefix.
	token, err := models.NewID()
	if err != nil {
		return nil, err
	}
	key := videoKeyPrefix(c, post) + token + videoContentTypeToExt(contentType)

	url, err := h.storage.PresignedPutURL(reqCtx(c), key, contentType, presignPutTTL)
	if err != nil {
		return nil, fmt.Errorf("post_attachments: presign put: %w", err)
	}
	return &presignVideoResponse{
		UploadURL: url,
		S3Key:     key,
		ExpiresIn: int(presignPutTTL / time.Second),
	}, nil
}

// videoKeyPrefix is the storage prefix a post's presigned video uploads live
// under.
func videoKeyPrefix(c *fiber.Ctx, post *models.Post) string {
	return storage.TenantKey(reqCtx(c), "post-attachments/"+post.ID+"/")
}

// videoTooLargeMessage words a size-cap reject in the largest whole unit.
func videoTooLargeMessage(maxBytes int64) string {
	if maxBytes >= 1<<30 {
		return fmt.Sprintf("video exceeds upload limit of %d GB", maxBytes>>30)
	}
	return fmt.Sprintf("video exceeds upload limit of %d MB", maxBytes>>20)
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
// @Description  weaker validation). The object's real size is checked against
// @Description  the media storage quota; over it, the object is deleted (402).
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string                true  "Post Sqid"
// @Param        body     body      finalizeVideoRequest  true  "Finalize body"
// @Success      201      {object}  attachmentResponse
// @Failure      400      {object}  map[string]string "too_large, empty_file, invalid_file, invalid request"
// @Failure      401      {object}  map[string]string
// @Failure      402      {object}  map[string]any    "media-storage limit reached"
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      415      {object}  map[string]string "unsupported_media_type"
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/finalize [post]
func (h *PostAttachmentsHandler) FinalizeVideo(c *fiber.Ctx) error {
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	var req finalizeVideoRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	att, err := h.finalizeVideoUpload(c, post, req.S3Key, req.AltText, maxVideoUploadBytes(), session, attachmentSourceEditor)
	if err != nil {
		return attachmentError(c, err)
	}
	return h.respondAttachment(c, post, att)
}

// finalizeVideoUpload turns an object PUT through a presigned URL into a post
// attachment: it proves the key belongs to the post, reads the object's real
// size, clears it against media_storage_bytes, probes it and saves the row,
// announcing it with source. A rejected object is deleted. Upload rejects come
// back as *attachmentReject.
func (h *PostAttachmentsHandler) finalizeVideoUpload(c *fiber.Ctx, post *models.Post, key, altText string, maxBytes int64, session *models.Session, source string) (*models.PostAttachment, error) {
	if h.storage == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "storage not configured")
	}
	if err := ensureMutable(post); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fiber.NewError(fiber.StatusBadRequest, "s3_key is required")
	}
	// Ownership: the key must sit under this tenant+post prefix, exactly as
	// presign minted it. This blocks finalizing an arbitrary object (or
	// another post's / tenant's upload).
	if !strings.HasPrefix(key, videoKeyPrefix(c, post)) {
		return nil, fiber.NewError(fiber.StatusBadRequest, "s3_key does not belong to this post")
	}
	altText, err := normalizeAltText(altText)
	if err != nil {
		return nil, err
	}
	info, err := h.statVideoUpload(reqCtx(c), key, maxBytes)
	if err != nil {
		return nil, err
	}
	quota, err := requireQuotaAmount(c, h.limiter, "media_storage_bytes", info.Size)
	if err != nil {
		_ = h.storage.Delete(reqCtx(c), key)
		return nil, err
	}

	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	att := &models.PostAttachment{
		ID:        id,
		PostID:    post.ID,
		AltText:   altText,
		SizeBytes: info.Size,
		S3Key:     key,
		CreatedBy: session.UserID,
	}

	probe, err := h.probeVideo(reqCtx(c), att)
	if err != nil {
		return nil, err
	}
	// Resolve the MIME so kind detection routes this to the video validator:
	// the probed container, else the stored content type, else the key
	// extension. No video type means the container is unsupported.
	att.MimeType = resolveVideoMIME(probe, info.ContentType, key)
	if !strings.HasPrefix(att.MimeType, "video/") {
		_ = h.storage.Delete(reqCtx(c), key)
		return nil, rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "unsupported video container or codec")
	}

	// The poster frame becomes the thumbnail, stored before the insert so
	// ThumbnailS3Key lands in one write.
	var poster []byte
	if probe != nil {
		poster = probe.PosterPNG
	}
	if err := h.saveAttachment(c, att, poster, quota, session.TenantID, source); err != nil {
		return nil, err
	}
	return att, nil
}

// statVideoUpload reads the uploaded object's authoritative size (never the
// client's claim), deleting an empty object or one over maxBytes.
func (h *PostAttachmentsHandler) statVideoUpload(ctx context.Context, key string, maxBytes int64) (*storage.ObjectInfo, error) {
	info, err := h.storage.Head(ctx, key)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "uploaded object not found; PUT the bytes to the presigned URL first")
	}
	if info.Size == 0 {
		_ = h.storage.Delete(ctx, key)
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeEmptyFile, "uploaded object is empty")
	}
	if info.Size > maxBytes {
		_ = h.storage.Delete(ctx, key)
		return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeTooLarge, videoTooLargeMessage(maxBytes))
	}
	return info, nil
}

// probeVideo probes the uploaded object via video-service and stamps the
// duration, codec and dimensions on att. Best-effort like pdf-service: a
// terminal "not a readable video" verdict deletes the object and rejects the
// upload; presign, transient or unreachable failures degrade to an unprobed
// attachment (nil result).
func (h *PostAttachmentsHandler) probeVideo(ctx context.Context, att *models.PostAttachment) (*video.ProbeResult, error) {
	if h.video == nil {
		return nil, nil
	}
	srcURL, err := h.storage.PresignedGetURL(ctx, att.S3Key, probeGetTTL)
	if err != nil {
		slog.WarnContext(ctx, "video probe presign failed", logging.AttrComponent, "post_attachments", logging.AttrError, err)
		return nil, nil
	}
	pctx, cancel := context.WithTimeout(ctx, videoProbeTimeout)
	res, err := h.video.Probe(pctx, video.ProbeOptions{
		SourceURL:    srcURL,
		RenderPoster: true,
		Filename:     path.Base(att.S3Key),
	})
	cancel()
	if err != nil {
		if video.IsInvalidVideo(err) {
			_ = h.storage.Delete(ctx, att.S3Key)
			return nil, rejectUpload(fiber.StatusBadRequest, models.UploadCodeInvalidFile, "uploaded file is not a readable video")
		}
		slog.WarnContext(ctx, "video probe failed", logging.AttrComponent, "post_attachments", "key", att.S3Key, logging.AttrError, err)
		return nil, nil
	}
	att.DurationMs = res.DurationMs
	att.Codec = res.Codec
	att.Width = res.Width
	att.Height = res.Height
	return res, nil
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
