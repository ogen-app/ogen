package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/plugins"
)

// Plugin request limits.
const (
	pluginRequestsPerMinute = 120
	maxNodeIDLen            = 100
	maxNodeNameLen          = 200
	maxFigmaFileNameLen     = 200
	maxFigmaFileKeyLen      = 100
	// maxPluginTitleLen bounds the asset title derived from a frame name.
	maxPluginTitleLen   = 120
	defaultPostPageSize = 20
)

// Codes for a send whose target post can't take it.
const (
	CodePostNotFound = "post_not_found"
	CodePostLocked   = "post_locked"
)

// CodeCampaignNotFound is the 404 for a campaign that was deleted or isn't in
// the token's workspace.
const CodeCampaignNotFound = "campaign_not_found"

// pluginSniffedExts maps the image types a design-tool export produces to the
// extension the asset is stored under. Detected from the bytes, not the
// client's filename.
var pluginSniffedExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
}

type pluginMeResponse struct {
	Workspace  pluginWorkspace          `json:"workspace"`
	User       pluginUser               `json:"user"`
	Connection pluginConnectionResponse `json:"connection"`
	Limits     pluginLimits             `json:"limits"`
}

type pluginLimits struct {
	MaxImageBytes     int64    `json:"max_image_bytes"`
	MaxVideoBytes     int64    `json:"max_video_bytes"`
	VideoContentTypes []string `json:"video_content_types"`
}

type pluginPostCampaign struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type pluginPost struct {
	ID              string              `json:"id"`
	Title           string              `json:"title"`
	Status          string              `json:"status"`
	Platform        string              `json:"platform"`
	Campaign        *pluginPostCampaign `json:"campaign"`
	UpdatedAt       time.Time           `json:"updated_at"`
	AttachmentCount int                 `json:"attachment_count"`
}

type pluginPostsResponse struct {
	Posts []pluginPost `json:"posts"`
}

type pluginPlatform struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type pluginCampaignPost struct {
	ID              string          `json:"id"`
	Title           string          `json:"title"`
	Status          string          `json:"status"`
	Platform        *pluginPlatform `json:"platform"`
	PostType        string          `json:"post_type"`
	ScheduledAt     *time.Time      `json:"scheduled_at"`
	AttachmentCount int             `json:"attachment_count"`
	VideoCount      int             `json:"video_count"`
	Attachable      bool            `json:"attachable"`
}

// pluginPlatformRules are the media rules of one platform, for checking a
// send before rendering it. Video is nil when the platform takes no video.
type pluginPlatformRules struct {
	Name      string                       `json:"name"`
	Video     *pluginVideoRules            `json:"video"`
	PostTypes []platforms.PostTypeRuleView `json:"post_types"`
}

type pluginVideoRules struct {
	MaxFileSizeBytes      int64    `json:"max_file_size_bytes"`
	AllowedFormats        []string `json:"allowed_formats"`
	MinDurationSeconds    int      `json:"min_duration_seconds"`
	MaxDurationSeconds    int      `json:"max_duration_seconds"`
	MaxWidth              int      `json:"max_width"`
	MaxHeight             int      `json:"max_height"`
	AllowedAspectRatios   []string `json:"allowed_aspect_ratios"`
	MaxAttachmentsPerPost int      `json:"max_attachments_per_post"`
	RequiresVideoTitle    bool     `json:"requires_video_title"`
}

type pluginCampaign struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Timezone  string     `json:"timezone"`
	StartDate *time.Time `json:"start_date"`
	EndDate   *time.Time `json:"end_date"`
	// PostsChangedAt moves forward whenever a post is created, deleted,
	// moved, rescheduled, retitled, or has its platform, post type or status
	// changed; null if the campaign never had a post.
	PostsChangedAt *time.Time           `json:"posts_changed_at"`
	Posts          []pluginCampaignPost `json:"posts"`
}

type pluginCampaignsResponse struct {
	Campaigns []pluginCampaign `json:"campaigns"`
	// Platforms holds the rules of every platform a listed post is on, by
	// platform id.
	Platforms map[string]pluginPlatformRules `json:"platforms"`
}

// pluginCampaignDetail is a campaign as GET /campaigns/{id} returns it: its
// posts carry their media.
type pluginCampaignDetail struct {
	pluginCampaign
	Posts []pluginCampaignDetailPost `json:"posts"`
}

type pluginCampaignDetailPost struct {
	pluginCampaignPost
	// Media lists the post's attachments: whole-post media first, then each
	// thread segment's, each by position.
	Media []pluginMedia `json:"media"`
}

type pluginCampaignResponse struct {
	Campaign pluginCampaignDetail `json:"campaign"`
	// Platforms holds the rules of every platform one of its posts is on, by
	// platform id.
	Platforms map[string]pluginPlatformRules `json:"platforms"`
}

type pluginAsset struct {
	ID        string                 `json:"id"`
	Title     string                 `json:"title"`
	Status    string                 `json:"status"`
	Origin    string                 `json:"origin"`
	OriginRef *models.AssetOriginRef `json:"origin_ref"`
	URL       string                 `json:"url,omitempty"`
}

type pluginAttachment struct {
	ID     string `json:"id"`
	PostID string `json:"post_id"`
}

// pluginImageResponse answers a send: asset for one stored in the content
// bank, attachment for one sent to a post. Exactly one of the two is set.
type pluginImageResponse struct {
	Asset        *pluginAsset      `json:"asset"`
	Deduplicated bool              `json:"deduplicated"`
	Attachment   *pluginAttachment `json:"attachment"`
	OpenURL      string            `json:"open_url"`
}

// limitPerToken throttles a plugin token's requests.
func (h *FigmaPluginHandler) limitPerToken(c *fiber.Ctx) error {
	tok, err := pluginTokenFrom(c)
	if err != nil {
		return err
	}
	if ok, retry := h.tokenLimiter.allow(tok.ID); !ok {
		return tooManyRequests(c, retry, "too many requests from this plugin, slow down")
	}
	return c.Next()
}

// Me godoc
// @Summary     Who the plugin acts as
// @Description The workspace and member a plugin token acts for, its connection, and the upload limits to check before exporting.
// @Tags        plugins
// @Produce     json
// @Security    PluginToken
// @Success     200 {object} pluginMeResponse
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Router      /api/plugins/figma/me [get]
func (h *FigmaPluginHandler) Me(c *fiber.Ctx) error {
	tok, err := pluginTokenFrom(c)
	if err != nil {
		return err
	}
	user, err := h.users.GetByIDWithTenant(reqCtx(c), tok.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return rejectPluginToken(c)
	}
	if err != nil {
		return err
	}
	workspace := pluginWorkspace{ID: tok.TenantID}
	if user.Tenant != nil {
		workspace.Name = user.Tenant.Name
	}
	return c.JSON(pluginMeResponse{
		Workspace:  workspace,
		User:       pluginUser{ID: user.ID, Name: user.Name},
		Connection: connectionResponse(tok),
		Limits: pluginLimits{
			MaxImageBytes:     maxImageUploadBytes(),
			MaxVideoBytes:     h.maxVideoBytes(),
			VideoContentTypes: pluginVideoContentTypes,
		},
	})
}

// ListPosts godoc
// @Summary     Posts the plugin can attach images to
// @Description Posts not yet submitted to a publisher, newest edit first, without their body text. q filters by title.
// @Tags        plugins
// @Produce     json
// @Security    PluginToken
// @Param       q     query string false "title contains (case-insensitive)"
// @Param       limit query int    false "1-50, default 20"
// @Success     200 {object} pluginPostsResponse
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Router      /api/plugins/figma/posts [get]
func (h *FigmaPluginHandler) ListPosts(c *fiber.Ctx) error {
	rows, err := h.posts.ListAttachTargets(reqCtx(c), c.Query("q"), c.QueryInt("limit", defaultPostPageSize))
	if err != nil {
		return err
	}
	out := pluginPostsResponse{Posts: make([]pluginPost, 0, len(rows))}
	for _, r := range rows {
		p := pluginPost{
			ID: r.ID, Title: r.Title, Status: string(r.Status), Platform: r.PlatformName,
			UpdatedAt: r.UpdatedAt, AttachmentCount: r.AttachmentCount,
		}
		if r.CampaignID != "" {
			p.Campaign = &pluginPostCampaign{ID: r.CampaignID, Name: r.CampaignName}
		}
		out.Posts = append(out.Posts, p)
	}
	return c.JSON(out)
}

// ListCampaigns godoc
// @Summary     Campaigns and their posts, for the plugin's "Send to" picker
// @Description Live campaigns (archived and deleted ones left out), active first, then scheduled, draft, paused and completed; newest start date first within a status, undated last. Up to 100 campaigns, each with up to 300 posts of every status in scheduled order (unscheduled last), without their body text. attachable is false for posts already submitted to a publisher. timezone is the campaign's IANA zone as stored ("" = UTC). post_type is the post's platform post type slug ("" until chosen); video_count counts its video attachments. posts_changed_at is when the campaign's posts last changed in a way a board shows (a post created, deleted, moved, rescheduled, retitled, or its platform, post type or status changed); null if it never had a post. platforms carries, by platform id, the media rules of every platform a listed post is on (video is null where the platform takes no video), for warning before a send; the server still validates.
// @Tags        plugins
// @Produce     json
// @Security    PluginToken
// @Success     200 {object} pluginCampaignsResponse
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Router      /api/plugins/figma/campaigns [get]
func (h *FigmaPluginHandler) ListCampaigns(c *fiber.Ctx) error {
	rows, err := h.posts.ListCampaignPostTree(reqCtx(c), repository.MaxTreeCampaigns, repository.MaxTreePostsPerCampaign)
	if err != nil {
		return err
	}
	out := pluginCampaignsResponse{Campaigns: make([]pluginCampaign, 0, len(rows))}
	used := map[string]bool{}
	for i := range rows {
		out.Campaigns = append(out.Campaigns, toPluginCampaign(&rows[i], used))
	}
	if out.Platforms, err = h.platformRules(c, used); err != nil {
		return err
	}
	return c.JSON(out)
}

// GetCampaign godoc
// @Summary     One campaign with all of its posts, for re-syncing a Figma board
// @Description The campaign in the same shape as one entry of GET /api/plugins/figma/campaigns, with every one of its posts (no cap) in scheduled order, unscheduled last. Archived campaigns are returned with their status. A post that isn't listed was deleted or moved to another campaign. platforms carries the media rules of every platform one of its posts is on. Each post also carries media: its attachments (kind image, video or pdf), whole-post media first, then each thread segment's (segment_index), each by position. preview_url is a presigned GET (valid 15 minutes) for a JPEG, PNG or GIF of at most 4096 px on the long edge, ready for figma.createImage: the image itself when it already qualifies, otherwise a scaled JPEG/PNG copy (made once, then reused); for a video, its poster. preview_url, preview_width and preview_height are null for PDFs, videos without a poster, and previews that couldn't be made (retried on the next read).
// @Tags        plugins
// @Produce     json
// @Security    PluginToken
// @Param       id path string true "campaign id"
// @Success     200 {object} pluginCampaignResponse
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Failure     404 {object} map[string]string "campaign_not_found: deleted, or not in this workspace"
// @Router      /api/plugins/figma/campaigns/{id} [get]
func (h *FigmaPluginHandler) GetCampaign(c *fiber.Ctx) error {
	row, err := h.posts.GetCampaignPostTree(reqCtx(c), c.Params("id"))
	if errors.Is(err, sql.ErrNoRows) {
		return rejectCode(c, fiber.StatusNotFound, CodeCampaignNotFound, "campaign not found in this workspace")
	}
	if err != nil {
		return err
	}
	used := map[string]bool{}
	camp := toPluginCampaign(row, used)
	postIDs := make([]string, len(camp.Posts))
	for i := range camp.Posts {
		postIDs[i] = camp.Posts[i].ID
	}
	media, err := h.media.mediaByPost(reqCtx(c), postIDs)
	if err != nil {
		return err
	}
	out := pluginCampaignResponse{Campaign: pluginCampaignDetail{
		pluginCampaign: camp, Posts: make([]pluginCampaignDetailPost, len(camp.Posts)),
	}}
	for i, p := range camp.Posts {
		m := media[p.ID]
		if m == nil {
			m = []pluginMedia{}
		}
		out.Campaign.Posts[i] = pluginCampaignDetailPost{pluginCampaignPost: p, Media: m}
	}
	if out.Platforms, err = h.platformRules(c, used); err != nil {
		return err
	}
	return c.JSON(out)
}

// toPluginCampaign maps a campaign tree to its wire shape, marking in used
// every platform one of its posts is on.
func toPluginCampaign(r *models.CampaignPostTree, used map[string]bool) pluginCampaign {
	camp := pluginCampaign{
		ID: r.ID, Name: r.Name, Status: string(r.Status), Timezone: r.Timezone,
		StartDate: r.StartDate, EndDate: r.EndDate, PostsChangedAt: r.PostsChangedAt,
		Posts: make([]pluginCampaignPost, 0, len(r.Posts)),
	}
	for _, p := range r.Posts {
		post := pluginCampaignPost{
			ID: p.ID, Title: p.Title, Status: string(p.Status), PostType: p.PlatformPostType, ScheduledAt: p.ScheduledAt,
			AttachmentCount: p.AttachmentCount, VideoCount: p.VideoCount, Attachable: !p.Status.IsSubmitted(),
		}
		if p.PlatformID != "" {
			post.Platform = &pluginPlatform{ID: p.PlatformID, Name: p.PlatformName}
			used[p.PlatformID] = true
		}
		camp.Posts = append(camp.Posts, post)
	}
	return camp
}

// platformRules returns the media rules of the platforms in ids. Disabled
// platforms are included: a post on one is still listed.
func (h *FigmaPluginHandler) platformRules(c *fiber.Ctx, ids map[string]bool) (map[string]pluginPlatformRules, error) {
	out := map[string]pluginPlatformRules{}
	if h.platforms == nil || len(ids) == 0 {
		return out, nil
	}
	all, err := h.platforms.List(reqCtx(c))
	if err != nil {
		return nil, err
	}
	for i := range all {
		p := &all[i]
		if !ids[p.ID] {
			continue
		}
		rules := pluginPlatformRules{Name: p.Name, PostTypes: platforms.ResolvePostTypeRules(p)}
		if v := p.VideoConstraints; !v.IsZero() {
			rules.Video = &pluginVideoRules{
				MaxFileSizeBytes: v.MaxFileSizeBytes, AllowedFormats: v.AllowedFormats,
				MinDurationSeconds: v.MinDurationSeconds, MaxDurationSeconds: v.MaxDurationSeconds,
				MaxWidth: v.MaxWidth, MaxHeight: v.MaxHeight, AllowedAspectRatios: v.AllowedAspectRatios,
				MaxAttachmentsPerPost: v.MaxAttachmentsPerPost, RequiresVideoTitle: v.RequiresVideoTitle,
			}
		}
		out[p.ID] = rules
	}
	return out, nil
}

// RevokeToken godoc
// @Summary     Disconnect this plugin
// @Description Revokes the calling plugin token (the plugin's "Disconnect").
// @Tags        plugins
// @Security    PluginToken
// @Success     204
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Router      /api/plugins/figma/token [delete]
func (h *FigmaPluginHandler) RevokeToken(c *fiber.Ctx) error {
	tok, err := pluginTokenFrom(c)
	if err != nil {
		return err
	}
	if err := h.pairing.Revoke(reqCtx(c), tok.TenantID, tok.ID); err != nil {
		return err
	}
	h.recordActivity(c, "plugin_revoked", tok.ID, map[string]any{"by": "plugin"})
	return c.SendStatus(fiber.StatusNoContent)
}

// pluginImageMeta is the frame description sent beside the image bytes.
type pluginImageMeta struct {
	ref     models.AssetOriginRef
	postID  string
	altText string
}

// SendImage godoc
// @Summary     Send a rendered frame to the content bank or a post
// @Description One PNG or JPEG per request (type detected from the bytes). Without post_id it is stored as a pending IMG asset with origin figma and processed like any upload (alt text, description, embedding); identical bytes return the existing asset with deduplicated=true. With post_id it is attached to that post only, like an upload in the post editor (metadata stripped, alt text generated unless sent), and never enters the content bank: asset is null, attachment is set and open_url is the post. A post that doesn't exist or was already sent for publishing is post_not_found / post_locked.
// @Tags        plugins
// @Accept      multipart/form-data
// @Produce     json
// @Security    PluginToken
// @Param       file      formData file   true  "PNG or JPEG bytes"
// @Param       node_id   formData string true  "Figma node id, e.g. 12:345"
// @Param       node_name formData string true  "frame name; becomes the asset title or attachment filename"
// @Param       file_name formData string false "Figma document name"
// @Param       file_key  formData string false "Figma file key (private builds only)"
// @Param       post_id   formData string false "attach to this post instead of the content bank"
// @Param       alt_text  formData string false "alt text written by the designer"
// @Success     201 {object} pluginImageResponse
// @Failure     400 {object} map[string]string "too_large, empty_file, invalid request"
// @Failure     401 {object} map[string]string "plugin_token_invalid"
// @Failure     402 {object} map[string]any    "content-bank or media-storage limit reached"
// @Failure     404 {object} map[string]string "post_not_found"
// @Failure     409 {object} map[string]string "post_locked"
// @Failure     415 {object} map[string]string "vector_rejected, unsupported_media_type"
// @Failure     429 {object} map[string]string
// @Failure     503 {object} map[string]string "service_unavailable"
// @Router      /api/plugins/figma/images [post]
func (h *FigmaPluginHandler) SendImage(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	tok, err := pluginTokenFrom(c)
	if err != nil {
		return err
	}
	meta, err := readPluginImageMeta(c)
	if err != nil {
		return err
	}
	in, err := h.readPluginImage(c, meta)
	if err != nil {
		return attachmentError(c, err)
	}
	if meta.postID != "" {
		return h.sendImageToPost(c, session, tok, meta.postID, in)
	}

	quota, err := h.assets.requireUploadQuota(c, int64(len(in.Raw)))
	if err != nil {
		return err
	}
	res := h.assets.ingestImage(c, session, in)
	if res.Status != "created" {
		return rejectCode(c, pluginUploadStatus(res.Code), res.Code, res.Error)
	}
	quota.dispatch(reqCtx(c))
	plugins.ImagesSent.Add(1)
	if res.deduplicated {
		plugins.ImagesDeduplicated.Add(1)
	}

	asset := pluginAssetFrom(res.Asset)
	h.recordActivity(c, "plugin_image_sent", tok.ID, map[string]any{"asset_id": res.Asset.ID, "deduplicated": res.deduplicated})
	return c.Status(fiber.StatusCreated).JSON(pluginImageResponse{
		Asset:        &asset,
		Deduplicated: res.deduplicated,
		OpenURL:      h.appBaseURL + "/content-bank/assets/" + res.Asset.ID,
	})
}

// sendImageToPost stores the frame as an attachment of the post the way the
// post editor's upload does. It never enters the content bank: a post's media
// is the post's own.
func (h *FigmaPluginHandler) sendImageToPost(c *fiber.Ctx, session *models.Session, tok *models.PluginToken, postID string, in imageIngest) error {
	post, err := h.pluginTargetPost(c, postID)
	if err != nil {
		return attachmentError(c, err)
	}
	if h.attachments.image == nil {
		return rejectAttachment(c, fiber.StatusServiceUnavailable, models.UploadCodeServiceUnavailable, "image processing is not configured")
	}
	quota, err := requireQuotaAmount(c, h.attachments.limiter, "media_storage_bytes", int64(len(in.Raw)))
	if err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	att := &models.PostAttachment{
		ID:        id,
		PostID:    post.ID,
		AltText:   in.AltText,
		CreatedBy: session.UserID,
		// Alt text the designer wrote is a manual edit: the async generator
		// never overwrites it.
		AltTextEditedByUser: in.AltText != "",
	}
	ext := strings.ToLower(filepath.Ext(in.Filename))
	if err := h.attachments.storeImage(reqCtx(c), att, in.Raw, in.MimeType, ext, in.Filename); err != nil {
		return attachmentError(c, err)
	}
	if err := h.attachments.saveAttachment(c, att, nil, quota, session.TenantID, attachmentSourceFigmaPlugin); err != nil {
		return err
	}
	plugins.ImagesSent.Add(1)
	h.recordActivity(c, "plugin_image_sent", tok.ID, map[string]any{"post_id": post.ID, "attachment_id": att.ID})
	return c.Status(fiber.StatusCreated).JSON(pluginImageResponse{
		Attachment: &pluginAttachment{ID: att.ID, PostID: post.ID},
		OpenURL:    h.appBaseURL + "/posts/" + post.ID,
	})
}

// readPluginImageMeta validates the form fields beside the file. Values are
// cloned: fasthttp reuses the request buffers they point into.
func readPluginImageMeta(c *fiber.Ctx) (pluginImageMeta, error) {
	field := func(name string, maxLen int, required bool) (string, error) {
		v := strings.TrimSpace(c.FormValue(name))
		if required && v == "" {
			return "", fiber.NewError(fiber.StatusBadRequest, name+" is required")
		}
		if utf8.RuneCountInString(v) > maxLen {
			return "", fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("%s exceeds %d characters", name, maxLen))
		}
		return strings.Clone(v), nil
	}
	var (
		m   pluginImageMeta
		err error
	)
	if m.ref.NodeID, err = field("node_id", maxNodeIDLen, true); err != nil {
		return m, err
	}
	if m.ref.NodeName, err = field("node_name", maxNodeNameLen, true); err != nil {
		return m, err
	}
	if m.ref.FileName, err = field("file_name", maxFigmaFileNameLen, false); err != nil {
		return m, err
	}
	if m.ref.FileKey, err = field("file_key", maxFigmaFileKeyLen, false); err != nil {
		return m, err
	}
	m.postID = strings.Clone(strings.TrimSpace(c.FormValue("post_id")))
	alt, err := normalizeAltText(c.FormValue("alt_text"))
	if err != nil {
		return m, err
	}
	m.altText = strings.Clone(alt)
	return m, nil
}

// readPluginImage reads the uploaded file, detects PNG/JPEG from its bytes and
// names it after the frame, then runs the content bank's image checks.
// Rejects come back as *attachmentReject.
func (h *FigmaPluginHandler) readPluginImage(c *fiber.Ctx, meta pluginImageMeta) (imageIngest, error) {
	fh, err := c.FormFile("file")
	if err != nil {
		return imageIngest{}, fiber.NewError(fiber.StatusBadRequest, "file is required")
	}
	raw, err := readFormFile(fh, maxImageUploadBytes())
	if err != nil {
		return imageIngest{}, err
	}
	if int64(len(raw)) > maxImageUploadBytes() {
		return imageIngest{}, rejectUpload(fiber.StatusBadRequest, models.UploadCodeTooLarge,
			fmt.Sprintf("file exceeds maximum size of %d MB", maxImageUploadBytes()>>20))
	}
	if len(raw) == 0 {
		return imageIngest{}, rejectUpload(fiber.StatusBadRequest, models.UploadCodeEmptyFile, "file is empty")
	}
	mime := http.DetectContentType(raw)
	ext, ok := pluginSniffedExts[mime]
	if !ok {
		if looksLikeSVG(fh.Filename, raw) {
			return imageIngest{}, rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeVectorRejected, "SVG is not supported — export the frame as PNG or JPG")
		}
		return imageIngest{}, rejectUpload(fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType, "only PNG and JPG exports are accepted")
	}
	filename := pluginImageFilename(meta.ref.NodeName, meta.ref.NodeID, ext)
	if _, fail := h.assets.checkImageUpload(filename, int64(len(raw))); fail != nil {
		return imageIngest{}, rejectUpload(pluginUploadStatus(fail.code), fail.code, fail.msg)
	}
	ref := meta.ref
	return imageIngest{
		Filename: filename, MimeType: mime, Raw: raw,
		Origin: models.AssetOriginFigma, OriginRef: &ref, AltText: meta.altText,
	}, nil
}

// pluginUploadStatus is the HTTP status for an upload failure code.
func pluginUploadStatus(code string) int {
	switch code {
	case models.UploadCodeServiceUnavailable:
		return fiber.StatusServiceUnavailable
	case models.UploadCodeInternalError:
		return fiber.StatusInternalServerError
	}
	return imageRejectStatus(code)
}

func pluginAssetFrom(a *models.Asset) pluginAsset {
	out := pluginAsset{ID: a.ID, Title: a.Title, Status: a.Status, Origin: a.Origin, OriginRef: a.OriginRef}
	if a.File != nil && a.File.URL != nil {
		out.URL = *a.File.URL
	}
	return out
}

// pluginImageFilename names an export after its frame. Figma names nest with
// "/" ("Hero / Desktop"), so path separators become "-" and control characters
// go; the result is trimmed of edge spaces, dots and dashes, cut to
// maxPluginTitleLen runes, and falls back to figma-<node id> when nothing is
// left.
func pluginImageFilename(nodeName, nodeID, ext string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, nodeName)
	name = truncateRunes(strings.Trim(name, " .-"), maxPluginTitleLen)
	if name == "" {
		name = "figma-" + strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(nodeID)
	}
	return name + ext
}

// looksLikeSVG reports an SVG by its name or its opening bytes.
func looksLikeSVG(filename string, raw []byte) bool {
	if strings.EqualFold(filepath.Ext(filename), ".svg") {
		return true
	}
	head := bytes.ToLower(bytes.TrimSpace(raw[:min(len(raw), 512)]))
	return bytes.HasPrefix(head, []byte("<svg")) || (bytes.HasPrefix(head, []byte("<?xml")) && bytes.Contains(head, []byte("<svg")))
}

func (h *FigmaPluginHandler) recordActivity(c *fiber.Ctx, typ, tokenID string, payload map[string]any) {
	if h.activity == nil {
		return
	}
	h.activity.Record(reqCtx(c), activity.CategoryIntegration, typ,
		activity.WithSource(activity.SourcePlugin),
		activity.WithEntity("plugin_connection", tokenID),
		activity.WithPayload(payload),
	)
}

// pluginTokenFrom returns the token RequirePluginToken authenticated.
func pluginTokenFrom(c *fiber.Ctx) (*models.PluginToken, error) {
	t, ok := c.Locals(pluginTokenLocal).(*models.PluginToken)
	if !ok || t == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "plugin token required")
	}
	return t, nil
}
