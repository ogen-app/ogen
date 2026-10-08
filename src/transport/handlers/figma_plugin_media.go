package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
)

// figma.createImage takes PNG, JPEG or GIF up to this many pixels on the long
// edge.
const figmaMaxImageEdge = 4096

// Preview rendering limits for one campaign read. Renders not started within
// the budget are skipped (preview_url null) and picked up by the next read.
const (
	previewRenderWorkers = 4
	previewRenderTimeout = 30 * time.Second
	previewRenderBudget  = 45 * time.Second
)

// figmaImageMimes are the formats figma.createImage decodes.
var figmaImageMimes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true}

// PreviewRenderer writes a scaled JPEG/PNG copy of a stored image. Implemented
// by *imageclient.Client.
type PreviewRenderer interface {
	RenderPreview(ctx context.Context, opts imageclient.RenderPreviewOptions) (*imageclient.RenderPreviewResult, error)
}

// pluginMedia is one attachment of a campaign post, with a copy of it the
// plugin can hand to figma.createImage.
type pluginMedia struct {
	ID           string `json:"id"`
	Kind         string `json:"kind" enums:"image,video,pdf"`
	Position     int    `json:"position"`
	SegmentIndex *int   `json:"segment_index" extensions:"x-nullable"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	// PreviewURL is a presigned GET for a JPEG, PNG or GIF of at most 4096 px
	// on the long edge: the image itself, a scaled copy of it, or a video's
	// poster. Null for PDFs, videos without a poster, and previews that
	// couldn't be made.
	PreviewURL    *string `json:"preview_url"    extensions:"x-nullable"`
	PreviewWidth  *int    `json:"preview_width"  extensions:"x-nullable"`
	PreviewHeight *int    `json:"preview_height" extensions:"x-nullable"`
}

// pluginMediaResolver lists campaign post media with Figma-ready previews.
// Copies are rendered by image-service once and recorded in media_previews.
type pluginMediaResolver struct {
	attachments repository.PostAttachmentRepository
	previews    repository.MediaPreviewRepository
	store       storage.Storage
	renderer    PreviewRenderer
}

// previewPlan is where one attachment's preview comes from: an object that
// already qualifies (key and its size), or a copy to look up or render from
// srcKey.
type previewPlan struct {
	postID string
	index  int
	key    string
	width  int
	height int
	copyOf *models.MediaPreviewKey
	srcKey string
}

// mediaByPost returns the media of every post in postIDs, by post id. Nil when
// attachments aren't wired.
func (r *pluginMediaResolver) mediaByPost(ctx context.Context, postIDs []string) (map[string][]pluginMedia, error) {
	if r == nil || r.attachments == nil || len(postIDs) == 0 {
		return nil, nil
	}
	atts, err := r.attachments.ListByPostIDs(ctx, postIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]pluginMedia, len(postIDs))
	var plans []previewPlan
	for i := range atts {
		a := &atts[i]
		kind := mediaKind(a.MimeType)
		if kind == "" {
			continue
		}
		if p, ok := planPreview(a, kind); ok {
			p.postID, p.index = a.PostID, len(out[a.PostID])
			plans = append(plans, p)
		}
		out[a.PostID] = append(out[a.PostID], pluginMedia{
			ID: a.ID, Kind: kind, Position: a.Position, SegmentIndex: a.SegmentIndex,
			Width: a.Width, Height: a.Height,
		})
	}
	r.attachPreviews(ctx, out, plans)
	return out, nil
}

// mediaKind is the plugin's kind for an attachment MIME type, "" if the
// plugin has no use for it.
func mediaKind(mime string) string {
	if k := platforms.AttachmentKind(mime); k != "" {
		return k
	}
	if strings.HasPrefix(mime, "image/") {
		return platforms.KindImage
	}
	return ""
}

// planPreview decides where an attachment's preview comes from. A video's
// poster is always copied: its stored size isn't recorded and may be rotated
// from the video's.
func planPreview(a *models.PostAttachment, kind string) (previewPlan, bool) {
	switch kind {
	case platforms.KindImage:
		if figmaImageMimes[a.MimeType] && a.Width > 0 && a.Height > 0 && max(a.Width, a.Height) <= figmaMaxImageEdge {
			return previewPlan{key: a.S3Key, width: a.Width, height: a.Height}, true
		}
		ref := a.ChecksumSHA256
		if ref == "" {
			ref = a.S3Key
		}
		return previewPlan{copyOf: &models.MediaPreviewKey{Source: models.MediaPreviewSourceImage, SourceRef: ref}, srcKey: a.S3Key}, true
	case platforms.KindVideo:
		if a.ThumbnailS3Key == "" {
			return previewPlan{}, false
		}
		return previewPlan{
			copyOf: &models.MediaPreviewKey{Source: models.MediaPreviewSourcePoster, SourceRef: a.ThumbnailS3Key},
			srcKey: a.ThumbnailS3Key,
		}, true
	}
	return previewPlan{}, false
}

// attachPreviews fills in the preview of every planned attachment: recorded
// copies are reused, missing ones rendered, and each signed for GET.
func (r *pluginMediaResolver) attachPreviews(ctx context.Context, out map[string][]pluginMedia, plans []previewPlan) {
	if r.store == nil || len(plans) == 0 {
		return
	}
	copies := r.copies(ctx, plans)
	for _, p := range plans {
		key, w, h := p.key, p.width, p.height
		if p.copyOf != nil {
			c, ok := copies[*p.copyOf]
			if !ok {
				continue
			}
			key, w, h = c.S3Key, c.Width, c.Height
		}
		url, err := r.store.PresignedGetURL(ctx, key, PresignedURLTTL)
		if err != nil {
			slog.WarnContext(ctx, "presign media preview failed", logging.AttrComponent, "figma_plugin", logging.AttrError, err)
			continue
		}
		m := &out[p.postID][p.index]
		m.PreviewURL, m.PreviewWidth, m.PreviewHeight = &url, &w, &h
	}
}

// copies returns the preview copies the plans need, rendering the ones not
// recorded yet.
func (r *pluginMediaResolver) copies(ctx context.Context, plans []previewPlan) map[models.MediaPreviewKey]models.MediaPreview {
	src := map[models.MediaPreviewKey]string{}
	keys := make([]models.MediaPreviewKey, 0, len(plans))
	for _, p := range plans {
		if p.copyOf != nil {
			if _, seen := src[*p.copyOf]; !seen {
				src[*p.copyOf] = p.srcKey
				keys = append(keys, *p.copyOf)
			}
		}
	}
	if len(keys) == 0 || r.previews == nil {
		return nil
	}
	found, err := r.previews.ListByKeys(ctx, figmaMaxImageEdge, keys)
	if err != nil {
		slog.WarnContext(ctx, "list media previews failed", logging.AttrComponent, "figma_plugin", logging.AttrError, err)
		return nil
	}
	var missing []models.MediaPreviewKey
	for _, k := range keys {
		if _, ok := found[k]; !ok {
			missing = append(missing, k)
		}
	}
	r.renderAll(ctx, missing, src, found)
	return found
}

// renderAll renders the missing copies with a few workers, adding each one
// made to found. Copies not started within previewRenderBudget are left for
// the next read.
func (r *pluginMediaResolver) renderAll(ctx context.Context, missing []models.MediaPreviewKey, src map[models.MediaPreviewKey]string, found map[models.MediaPreviewKey]models.MediaPreview) {
	if r.renderer == nil || len(missing) == 0 {
		return
	}
	scheduling, cancel := context.WithTimeout(ctx, previewRenderBudget)
	defer cancel()
	jobs := make(chan models.MediaPreviewKey)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for range min(previewRenderWorkers, len(missing)) {
		wg.Go(func() {
			for k := range jobs {
				p, err := r.render(ctx, k, src[k])
				if err != nil {
					slog.WarnContext(ctx, "render media preview failed", logging.AttrComponent, "figma_plugin",
						"source", k.Source, logging.AttrError, err)
					continue
				}
				mu.Lock()
				found[k] = *p
				mu.Unlock()
			}
		})
	}
	// A worker may be busy, so the send itself must give up at the budget.
schedule:
	for _, k := range missing {
		select {
		case jobs <- k:
		case <-scheduling.Done():
			break schedule
		}
	}
	close(jobs)
	wg.Wait()
}

// render has image-service write a copy of srcKey and records it.
func (r *pluginMediaResolver) render(ctx context.Context, k models.MediaPreviewKey, srcKey string) (*models.MediaPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, previewRenderTimeout)
	defer cancel()
	dest := previewKey(ctx, k, figmaMaxImageEdge)
	getURL, err := r.store.PresignedGetURL(ctx, srcKey, PresignedURLTTL)
	if err != nil {
		return nil, err
	}
	putURL, err := r.store.PresignedPutURL(ctx, dest, "", PresignedURLTTL)
	if err != nil {
		return nil, err
	}
	res, err := r.renderer.RenderPreview(ctx, imageclient.RenderPreviewOptions{
		SourceURL: getURL, DestPutURL: putURL, MaxLongEdge: figmaMaxImageEdge,
	})
	if err != nil {
		return nil, err
	}
	p := &models.MediaPreview{
		Source: k.Source, SourceRef: k.SourceRef, MaxLongEdge: figmaMaxImageEdge, S3Key: dest,
		MimeType: res.Mime, Width: res.Width, Height: res.Height, SizeBytes: res.SizeBytes,
	}
	if err := r.previews.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// previewKey is where the copy for k is stored, under the tenant's prefix.
// The same key is rewritten if two reads render it at once.
func previewKey(ctx context.Context, k models.MediaPreviewKey, maxLongEdge int) string {
	sum := sha256.Sum256([]byte(k.Source + "\x00" + k.SourceRef))
	return storage.TenantKey(ctx, fmt.Sprintf("media-previews/%s-%d", hex.EncodeToString(sum[:]), maxLongEdge))
}
