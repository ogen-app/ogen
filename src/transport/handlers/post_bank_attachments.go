package handlers

import (
	"context"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
)

// BankAssetReader loads a content-bank asset with its file row hydrated.
// Implemented by repository.AssetRepository; tenant scoping comes from its hooks.
type BankAssetReader interface {
	GetByID(ctx context.Context, id string) (*models.Asset, error)
}

// WithContentBank enables attaching content-bank images to posts.
func (h *PostAttachmentsHandler) WithContentBank(assets BankAssetReader) *PostAttachmentsHandler {
	h.bank = assets
	return h
}

// attachFromAssetRequest names the bank image to attach. alt_text, when present,
// replaces the asset's own; segment_index places the media on a thread segment.
type attachFromAssetRequest struct {
	AssetID      string  `json:"asset_id"`
	AltText      *string `json:"alt_text"`
	SegmentIndex *int    `json:"segment_index"`
}

// AttachFromAsset godoc
// @Summary      Attach a content-bank image to a post
// @Description  Creates a post attachment from an existing `IMG` asset without
// @Description  re-uploading. The bank keeps the original upload (EXIF and
// @Description  all), so its bytes go through the same metadata strip a direct
// @Description  upload gets and land as the attachment's own object: deleting
// @Description  the asset later leaves the post intact. The asset's alt text
// @Description  and its user-edited flag carry over unless `alt_text` is sent;
// @Description  an asset without alt text gets it generated as on upload. The
// @Description  copy counts toward media_storage_bytes (402 when over).
// @Description  Per-platform caps are soft warnings in the response, exactly as
// @Description  on upload. Image rejects use the upload's `{code, error}` shape.
// @Tags         post-attachments
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path      string                  true  "Post Sqid"
// @Param        body     body      attachFromAssetRequest  true  "Asset to attach"
// @Success      201      {object}  attachmentResponse
// @Failure      400      {object}  map[string]string
// @Failure      401      {object}  map[string]string
// @Failure      402      {object}  map[string]any     "media_storage_bytes limit reached"
// @Failure      404      {object}  map[string]string  "post or asset not found"
// @Failure      409      {object}  map[string]string  "post is in a terminal publishing state"
// @Failure      415      {object}  map[string]string
// @Failure      422      {object}  map[string]string  "asset is not an image"
// @Failure      503      {object}  map[string]string
// @Router       /api/posts/{post_id}/attachments/from-asset [post]
func (h *PostAttachmentsHandler) AttachFromAsset(c *fiber.Ctx) error {
	if h.storage == nil || h.bank == nil {
		return rejectAttachment(c, fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "content bank attachments are not configured")
	}
	post, err := loadParam(c, "post_id", h.postRepo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if err := ensureMutable(post); err != nil {
		return err
	}

	var req attachFromAssetRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	att, err := h.attachBankAsset(c, post, &req, session)
	if err != nil {
		return attachmentError(c, err)
	}
	return h.respondAttachment(c, post, att)
}

// attachBankAsset copies a content-bank image onto post as a new attachment:
// it validates the request, strips the bank original's metadata into the
// attachment's own object, gates media_storage_bytes on the stripped size and
// persists the row. The caller has checked the post is mutable. Image rejects
// come back as *attachmentReject.
func (h *PostAttachmentsHandler) attachBankAsset(c *fiber.Ctx, post *models.Post, req *attachFromAssetRequest, session *models.Session) (*models.PostAttachment, error) {
	att, err := newBankAttachment(post, req, session.UserID)
	if err != nil {
		return nil, err
	}
	file, err := h.loadBankImage(reqCtx(c), req.AssetID, att, req.AltText == nil)
	if err != nil {
		return nil, err
	}

	if h.image == nil {
		return nil, rejectUpload(fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "image processing is not configured")
	}
	cleanKey := attachmentKey(reqCtx(c), att, strings.ToLower(filepath.Ext(file.S3Key)))
	prep, err := h.stripImage(reqCtx(c), file.S3Key, cleanKey, file.MimeType, file.OriginalName)
	if err != nil {
		return nil, err
	}
	// Gate on the stripped copy's size — what the attachment stores and the
	// usage counter sums — not the bank original's. Nothing was uploaded, so
	// the only cost of checking after the strip is dropping the copy on a deny.
	quota, err := requireQuotaAmount(c, h.limiter, "media_storage_bytes", prep.SizeBytes)
	if err != nil {
		_ = h.storage.Delete(reqCtx(c), cleanKey)
		return nil, err
	}
	att.MimeType = prep.Mime
	att.SizeBytes = prep.SizeBytes
	att.Width = prep.Width
	att.Height = prep.Height
	att.IsAnimated = prep.IsAnimated
	att.ChecksumSHA256 = prep.ChecksumSHA256
	att.S3Key = cleanKey
	if err := h.saveAttachment(c, att, nil, quota, session.TenantID); err != nil {
		return nil, err
	}
	return att, nil
}

// newBankAttachment validates the request against the post and builds the row.
// A sent alt_text follows upload semantics: non-empty marks it user-edited.
func newBankAttachment(post *models.Post, req *attachFromAssetRequest, createdBy string) (*models.PostAttachment, error) {
	if strings.TrimSpace(req.AssetID) == "" {
		return nil, fiber.NewError(fiber.StatusBadRequest, "asset_id is required")
	}
	if req.SegmentIndex != nil {
		if *req.SegmentIndex < 0 {
			return nil, fiber.NewError(fiber.StatusBadRequest, "segment_index must be a non-negative integer")
		}
		if !post.IsThread() {
			return nil, fiber.NewError(fiber.StatusUnprocessableEntity, "segment_index is only valid on a thread post")
		}
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	att := &models.PostAttachment{
		ID:           id,
		PostID:       post.ID,
		SegmentIndex: req.SegmentIndex,
		CreatedBy:    createdBy,
	}
	if req.AltText != nil {
		alt, err := normalizeAltText(*req.AltText)
		if err != nil {
			return nil, err
		}
		att.AltText = alt
		att.AltTextEditedByUser = alt != ""
	}
	return att, nil
}

// loadBankImage resolves the asset to its stored image file. With inheritAlt it
// copies the asset's alt text and edited flag, so a user's wording is never
// demoted to a generated one, nor a generated one promoted.
func (h *PostAttachmentsHandler) loadBankImage(ctx context.Context, assetID string, att *models.PostAttachment, inheritAlt bool) (*models.AssetFile, error) {
	asset, err := h.bank.GetByID(ctx, assetID)
	if err != nil {
		return nil, notFound(err, "asset not found")
	}
	if asset.Type == nil || *asset.Type != models.AssetTypeImage {
		return nil, fiber.NewError(fiber.StatusUnprocessableEntity, "asset is not an image")
	}
	if asset.File == nil || asset.File.S3Key == "" {
		return nil, fiber.NewError(fiber.StatusUnprocessableEntity, "image asset has no stored file")
	}
	if inheritAlt {
		att.AltText = truncateRunes(strings.TrimSpace(asset.AltText), maxAltTextLen())
		att.AltTextEditedByUser = asset.AltTextEditedByUser && att.AltText != ""
	}
	return asset.File, nil
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
