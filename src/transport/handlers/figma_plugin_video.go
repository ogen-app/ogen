package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/usecase/plugins"
)

// pluginVideoContentTypes are the containers a design tool's video export
// produces.
var pluginVideoContentTypes = []string{"video/mp4", "video/webm"}

// pluginPostLevelRules are the whole-post rules a new video can break: too
// many media for the platform or post type, or a mix the platform refuses.
// Other whole-post rules (content, title, link) aren't about the upload.
var pluginPostLevelRules = []string{
	platforms.RuleMaxAttachmentsCount,
	platforms.RuleMaxAttachments,
	platforms.RuleAttachmentMix,
}

type pluginPresignVideoRequest struct {
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type pluginFinalizeVideoRequest struct {
	S3Key    string `json:"s3_key"`
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	FileName string `json:"file_name"`
	AltText  string `json:"alt_text"`
}

type pluginVideoAttachment struct {
	ID           string `json:"id"`
	PostID       string `json:"post_id"`
	MimeType     string `json:"mime_type"`
	DurationMs   int64  `json:"duration_ms"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	SizeBytes    int64  `json:"size_bytes"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
}

type pluginVideoResponse struct {
	Attachment         pluginVideoAttachment       `json:"attachment"`
	PlatformValidation []platforms.ValidationError `json:"platform_validation"`
	OpenURL            string                      `json:"open_url"`
}

// PresignVideo godoc
// @Summary     Start sending a rendered video to a post
// @Description Returns a presigned PUT URL valid for 30 minutes. PUT the MP4 or WebM bytes to upload_url with the same Content-Type and no Authorization header, then call finalize with s3_key. size_bytes is checked against limits.max_video_bytes from /me and against the workspace's media storage quota.
// @Tags        plugins
// @Accept      json
// @Produce     json
// @Security    PluginToken
// @Param       post_id path string                    true "post to attach the video to"
// @Param       body    body pluginPresignVideoRequest true "declared upload"
// @Success     200 {object} presignVideoResponse
// @Failure     400 {object} map[string]string "too_large, invalid request"
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Failure     402 {object} map[string]any    "media-storage limit reached"
// @Failure     404 {object} map[string]string "post_not_found"
// @Failure     409 {object} map[string]string "post_locked"
// @Failure     415 {object} map[string]string "unsupported_media_type"
// @Failure     429 {object} map[string]string
// @Failure     503 {object} map[string]string
// @Router      /api/plugins/figma/posts/{post_id}/videos/presign [post]
func (h *FigmaPluginHandler) PresignVideo(c *fiber.Ctx) error {
	post, err := h.pluginTargetPost(c)
	if err != nil {
		return attachmentError(c, err)
	}
	var req pluginPresignVideoRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	ct := strings.TrimSpace(strings.ToLower(req.ContentType))
	if !slices.Contains(pluginVideoContentTypes, ct) {
		return rejectCode(c, fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "only MP4 and WebM videos are accepted")
	}
	out, err := h.attachments.presignVideoUpload(c, post, ct, req.SizeBytes, h.maxVideoBytes())
	if err != nil {
		return attachmentError(c, err)
	}
	return c.JSON(out)
}

// FinalizeVideo godoc
// @Summary     Attach an uploaded video to a post
// @Description Probes the object PUT through the presigned URL (duration, size, poster frame) and attaches it to the post. platform_validation lists the platform and post-type rules this video breaks, as warnings; the attachment is still created. Repeating a finalize for an s3_key already attached returns that attachment with 200, even once the post has been sent for publishing. A refused object is deleted.
// @Tags        plugins
// @Accept      json
// @Produce     json
// @Security    PluginToken
// @Param       post_id path string                     true "post the video was presigned for"
// @Param       body    body pluginFinalizeVideoRequest true "the presigned key and the frame it was rendered from"
// @Success     200 {object} pluginVideoResponse "already attached"
// @Success     201 {object} pluginVideoResponse
// @Failure     400 {object} map[string]string "too_large, empty_file, invalid_file, invalid request"
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Failure     402 {object} map[string]any    "media-storage limit reached"
// @Failure     404 {object} map[string]string "post_not_found"
// @Failure     409 {object} map[string]string "post_locked"
// @Failure     415 {object} map[string]string "unsupported_media_type"
// @Failure     429 {object} map[string]string
// @Failure     503 {object} map[string]string
// @Router      /api/plugins/figma/posts/{post_id}/videos/finalize [post]
func (h *FigmaPluginHandler) FinalizeVideo(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	tok, err := pluginTokenFrom(c)
	if err != nil {
		return err
	}
	post, err := h.pluginPost(c)
	if err != nil {
		return attachmentError(c, err)
	}
	req, err := readPluginFinalizeVideo(c)
	if err != nil {
		return err
	}

	// An already-attached key answers before the lock check: a retry whose
	// first response was lost still gets its attachment after the post was
	// scheduled.
	existing, err := h.attachments.repo.ListByPostID(reqCtx(c), post.ID)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(existing, func(a models.PostAttachment) bool { return a.S3Key == req.S3Key }); i >= 0 {
		return c.JSON(h.videoResponse(c, post, &existing[i], existing))
	}
	if ensureMutable(post) != nil {
		return attachmentError(c, errPluginPostLocked())
	}

	att, err := h.attachments.finalizeVideoUpload(c, post, req.S3Key, req.AltText, h.maxVideoBytes(), session, attachmentSourceFigmaPlugin)
	if err != nil {
		plugins.VideosRejected.Add(1)
		return attachmentError(c, err)
	}
	plugins.VideosSent.Add(1)
	h.recordActivity(c, "plugin_video_sent", tok.ID, map[string]any{
		"post_id": post.ID, "attachment_id": att.ID,
		"duration_ms": att.DurationMs, "size_bytes": att.SizeBytes,
		"node_id": req.NodeID, "node_name": req.NodeName, "file_name": req.FileName,
	})
	return c.Status(fiber.StatusCreated).JSON(h.videoResponse(c, post, att, append(existing, *att)))
}

// pluginPost loads the :post_id post, answering post_not_found when this
// workspace has no such post.
func (h *FigmaPluginHandler) pluginPost(c *fiber.Ctx) (*models.Post, error) {
	post, err := h.posts.GetByID(reqCtx(c), c.Params("post_id"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, rejectUpload(fiber.StatusNotFound, CodePostNotFound, "post not found in this workspace")
	}
	return post, err
}

// pluginTargetPost is pluginPost refusing a post already sent for publishing.
func (h *FigmaPluginHandler) pluginTargetPost(c *fiber.Ctx) (*models.Post, error) {
	post, err := h.pluginPost(c)
	if err != nil {
		return nil, err
	}
	if ensureMutable(post) != nil {
		return nil, errPluginPostLocked()
	}
	return post, nil
}

// errPluginPostLocked is the post_locked reject for new media on a post
// already sent for publishing.
func errPluginPostLocked() error {
	return rejectUpload(fiber.StatusConflict, CodePostLocked, "the post was already sent for publishing and can't take new media")
}

// readPluginFinalizeVideo parses and bounds the finalize body.
func readPluginFinalizeVideo(c *fiber.Ctx) (pluginFinalizeVideoRequest, error) {
	var req pluginFinalizeVideoRequest
	if err := c.BodyParser(&req); err != nil {
		return req, fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	req.S3Key = strings.TrimSpace(req.S3Key)
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.NodeName = strings.TrimSpace(req.NodeName)
	req.FileName = strings.TrimSpace(req.FileName)
	for _, f := range []struct {
		name, value string
		maxLen      int
		required    bool
	}{
		{"node_id", req.NodeID, maxNodeIDLen, true},
		{"node_name", req.NodeName, maxNodeNameLen, true},
		{"file_name", req.FileName, maxFigmaFileNameLen, false},
	} {
		if f.required && f.value == "" {
			return req, fiber.NewError(fiber.StatusBadRequest, f.name+" is required")
		}
		if utf8.RuneCountInString(f.value) > f.maxLen {
			return req, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("%s exceeds %d characters", f.name, f.maxLen))
		}
	}
	return req, nil
}

// maxVideoBytes is the plugin's video cap: the configured plugin limit, never
// above the web app's.
func (h *FigmaPluginHandler) maxVideoBytes() int64 {
	if h.maxPluginVideoBytes > 0 {
		return min(h.maxPluginVideoBytes, maxVideoUploadBytes())
	}
	return maxVideoUploadBytes()
}

// videoResponse describes att and the rules it breaks on its post, given the
// post's attachments including att.
func (h *FigmaPluginHandler) videoResponse(c *fiber.Ctx, post *models.Post, att *models.PostAttachment, atts []models.PostAttachment) pluginVideoResponse {
	h.attachments.hydratePresigned(c, att)
	return pluginVideoResponse{
		Attachment: pluginVideoAttachment{
			ID: att.ID, PostID: att.PostID, MimeType: att.MimeType, DurationMs: att.DurationMs,
			Width: att.Width, Height: att.Height, SizeBytes: att.SizeBytes, ThumbnailURL: att.ThumbnailURL,
		},
		PlatformValidation: videoValidation(post, att.ID, atts),
		OpenURL:            h.appBaseURL + "/posts/" + post.ID,
	}
}

// videoValidation is what the post's publish gate would say about one
// attachment: its own rule failures plus the whole-post media rules it
// breaks. Never nil, so the plugin always gets a list.
func videoValidation(post *models.Post, attID string, atts []models.PostAttachment) []platforms.ValidationError {
	out := []platforms.ValidationError{}
	if post.Platform == nil {
		return out
	}
	for _, e := range platforms.ValidatePublishReadiness(post, post.Platform, atts)[post.Platform.ID] {
		if e.AttachmentID == attID || (e.AttachmentID == "" && slices.Contains(pluginPostLevelRules, e.Rule)) {
			out = append(out, e)
		}
	}
	return out
}
