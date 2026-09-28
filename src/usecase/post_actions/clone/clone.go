// Package clone implements the shared "clone a Post" operation.
// It is the single source of truth used by both the REST
// endpoint (POST /api/posts/:id/clone) and the Post Assistant's
// clonePost tool, so the two entry points can never drift.
//
// A clone is always a fresh draft in the source's campaign and phase.
// Content is copied verbatim unless the caller supplies an override
// (the assistant passes AI-adapted content when cloning across
// platforms). Attachments are deep-copied in object storage so the
// clone and its source have fully independent blob lifecycles.
package clone

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/usecase/post_actions/internal/postaudit"
)

// ErrSourceNotFound is returned when the post being cloned does not exist.
var ErrSourceNotFound = errors.New("source post not found")

// ErrInvalidPlatform is returned when a requested target platform does
// not exist or has no usable post types.
var ErrInvalidPlatform = errors.New("invalid target platform")

// ErrStorageUnavailable is returned when the source has attachments to
// copy but no object storage is configured — cloning would otherwise
// create attachment rows pointing at blobs that were never copied.
var ErrStorageUnavailable = errors.New("cannot clone attachments without object storage")

// Trigger identifies which entry point initiated a clone (recorded in
// the audit log).
const (
	TriggerAPI       = postaudit.TriggerAPI
	TriggerAssistant = postaudit.TriggerAssistant
)

// Options controls a single clone. The zero value copies nothing; use
// DefaultOptions to start from a full copy.
type Options struct {
	// Actor is the user id (or models.ActorSystem) recorded as the
	// clone's creator and on the audit entry. Required.
	Actor string
	// Trigger is TriggerAPI or TriggerAssistant.
	Trigger string

	// TargetPlatformID overrides the platform; "" keeps the source's.
	TargetPlatformID string
	// TargetPostType overrides the post type; "" keeps the source's (or
	// falls back to the target platform's default when incompatible).
	TargetPostType string
	// ContentOverride replaces the cloned content when non-nil. The
	// assistant sets this to platform-adapted content.
	ContentOverride *string
	// TitleOverride replaces the title when non-nil. When nil, the title
	// convention applies (unchanged same-platform; "<title> (Platform)"
	// cross-platform).
	TitleOverride *string

	CopyMedia  bool
	CopyAssets bool
	CopyCTA    bool
	CopyPhase  bool
}

// DefaultOptions returns Options that copy everything (media, assets,
// CTA, phase) — the standard "duplicate this post" behaviour.
func DefaultOptions(actor, trigger string) Options {
	return Options{
		Actor:      actor,
		Trigger:    trigger,
		CopyMedia:  true,
		CopyAssets: true,
		CopyCTA:    true,
		CopyPhase:  true,
	}
}

// Result is the outcome of a clone.
type Result struct {
	// Post is the newly created draft.
	Post *models.Post
	// Adapted reports whether the cloned content differs from the source
	// (true when ContentOverride changed it).
	Adapted bool
	// PostTypeFellBack is true when a requested target post type was
	// invalid for the target platform and was replaced with the
	// platform default; the caller should tell the user.
	PostTypeFellBack bool
	// ResolvedPostType is the post type actually assigned to the clone.
	ResolvedPostType string
}

// Service performs clones. Construct with New.
type Service struct {
	db          *bun.DB
	posts       repository.PostRepository
	versions    repository.PostVersionRepository
	attachments repository.PostAttachmentRepository
	platforms   repository.PlatformRepository
	logs        repository.PostLogRepository
	store       storage.Storage
	hub         eventhub.Hub
}

// New wires a clone Service. store and hub may be nil (uploads /
// eventing disabled); logs may be nil (audit disabled).
func New(
	db *bun.DB,
	posts repository.PostRepository,
	versions repository.PostVersionRepository,
	attachments repository.PostAttachmentRepository,
	platforms repository.PlatformRepository,
	logs repository.PostLogRepository,
	store storage.Storage,
	hub eventhub.Hub,
) *Service {
	return &Service{
		db:          db,
		posts:       posts,
		versions:    versions,
		attachments: attachments,
		platforms:   platforms,
		logs:        logs,
		store:       store,
		hub:         hub,
	}
}

// target is the resolved platform and post type a clone lands on.
type target struct {
	platformID string
	platform   *models.Platform // nil for a platform-less draft
	postType   string
	fellBack   bool
}

// Clone duplicates the post identified by sourceID per opts and returns
// the new draft. The DB writes (post, attachment rows, v1 version,
// audit entry) commit in one transaction; storage copies happen before
// the transaction and are cleaned up if anything after them fails.
func (s *Service) Clone(ctx context.Context, sourceID string, opts Options) (*Result, error) {
	src, err := s.posts.GetByID(ctx, sourceID)
	if err != nil {
		return nil, postaudit.NotFound(err, ErrSourceNotFound)
	}
	tgt, err := s.resolveTarget(ctx, src, opts)
	if err != nil {
		return nil, err
	}

	content := src.Content
	if opts.ContentOverride != nil {
		content = *opts.ContentOverride
	}
	adapted := content != src.Content
	// An adapted body would diverge from the stored segments, and a
	// retarget to a single-message type demotes the clone.
	keepThread := tgt.postType == models.PostTypeThread && !adapted

	newID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	clone := buildClone(src, tgt, opts, newID, content, keepThread, now)

	var copiedKeys []string
	committed := false
	defer func() {
		if !committed {
			s.cleanupKeys(context.Background(), copiedKeys)
		}
	}()

	// Blobs are copied before the transaction so the rows reference keys
	// that already exist.
	newAtts, copiedKeys, err := s.copyAttachments(ctx, src.ID, newID, opts, now, keepThread)
	if err != nil {
		return nil, err
	}
	versionID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	version := &models.PostVersion{
		ID:            versionID,
		PostID:        newID,
		VersionNumber: 1,
		Content:       content,
		Note:          fmt.Sprintf("Cloned from #%s", src.ID),
		Creator:       postaudit.CreatorFor(opts.Trigger),
	}
	logEntry, err := postaudit.NewLog(newID, models.PostLogEventPostCloned, opts.Actor, "post cloned from #"+src.ID, map[string]any{
		"source_post_id":  src.ID,
		"new_post_id":     newID,
		"target_platform": tgt.platformID,
		"adapted":         adapted,
		"trigger":         opts.Trigger,
	})
	if err != nil {
		return nil, err
	}
	if err := s.insert(ctx, clone, newAtts, version, logEntry); err != nil {
		return nil, err
	}
	committed = true

	// Dotted bus wire type; the persisted post_logs / activity taxonomy
	// stays "post_cloned".
	postaudit.Publish(s.hub, newID, "post.cloned", opts.Actor, map[string]any{
		"sourcePostId": src.ID,
		"newPostId":    newID,
		"adapted":      adapted,
	})

	return &Result{
		Post:             clone,
		Adapted:          adapted,
		PostTypeFellBack: tgt.fellBack,
		ResolvedPostType: tgt.postType,
	}, nil
}

// resolveTarget picks the clone's platform and post type. An unknown
// platform or one without post types yields ErrInvalidPlatform; a
// requested post type the platform lacks falls back to its default.
func (s *Service) resolveTarget(ctx context.Context, src *models.Post, opts Options) (target, error) {
	tgt := target{platformID: cmp.Or(opts.TargetPlatformID, src.PlatformID)}
	if tgt.platformID == "" {
		return tgt, nil
	}
	p, err := s.platforms.GetByID(ctx, tgt.platformID)
	if err != nil {
		return target{}, postaudit.NotFound(err, fmt.Errorf("%w: %s", ErrInvalidPlatform, tgt.platformID))
	}
	tgt.platform = p
	tgt.postType = cmp.Or(opts.TargetPostType, src.PlatformPostType)
	if _, ok := p.PostTypes[tgt.postType]; tgt.postType != "" && ok {
		return tgt, nil
	}
	def := defaultPostType(p.PostTypes)
	if def == "" {
		return target{}, fmt.Errorf("%w: %s has no post types", ErrInvalidPlatform, tgt.platformID)
	}
	// Only a requested (non-empty) type that didn't fit is worth reporting.
	tgt.fellBack = tgt.postType != "" && tgt.postType != def
	tgt.postType = def
	return tgt, nil
}

// buildClone assembles the new draft row from src per opts.
func buildClone(src *models.Post, tgt target, opts Options, newID, content string, keepThread bool, now time.Time) *models.Post {
	title := src.Title
	switch {
	case opts.TitleOverride != nil:
		title = *opts.TitleOverride
	case tgt.platform != nil && tgt.platformID != src.PlatformID:
		title = src.Title + " (" + tgt.platform.Name + ")"
	}

	clone := &models.Post{
		ID:               newID,
		CampaignID:       src.CampaignID,
		PlatformID:       tgt.platformID,
		PlatformPostType: tgt.postType,
		Title:            title,
		Content:          content,
		MediaURLs:        models.StringSlice{},
		Status:           models.PostStatusDraft,
		CTAType:          models.CTATypeNone,
		UsedAssetIDs:     models.StringSlice{},
		ClonedFromPostID: &src.ID,
		CreatedBy:        opts.Actor,
		CreatedAt:        now,
		UpdatedAt:        now,
		UsedAssets:       []models.Asset{},
	}
	if keepThread {
		clone.ThreadSegments = append(models.ThreadSegments{}, src.ThreadSegments...)
	}
	if opts.CopyMedia {
		clone.MediaURLs = append(models.StringSlice{}, src.MediaURLs...)
	}
	if opts.CopyAssets {
		clone.UsedAssetIDs = append(models.StringSlice{}, src.UsedAssetIDs...)
	}
	if opts.CopyCTA {
		clone.CTAType = src.CTAType
		clone.CTAUrl = src.CTAUrl
		clone.TargetAudienceNotes = src.TargetAudienceNotes
	}
	if opts.CopyPhase {
		clone.CampaignTypePhaseID = src.CampaignTypePhaseID
	}
	return clone
}

// insert writes the clone, its attachments, its v1 version and the
// audit entry in one transaction.
func (s *Service) insert(ctx context.Context, clone *models.Post, atts []*models.PostAttachment, version *models.PostVersion, logEntry *models.PostLog) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(clone).Exec(ctx); err != nil {
			return err
		}
		for _, a := range atts {
			if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.NewInsert().Model(version).Exec(ctx); err != nil {
			return err
		}
		if s.logs == nil {
			return nil
		}
		return s.logs.AppendTx(ctx, tx, logEntry)
	})
}

// copyAttachments duplicates the source post's attachments to newPostID,
// copying each blob (and thumbnail) to a fresh key. Returns the new
// rows (to be inserted in the caller's transaction) and the keys
// written, which the caller deletes if a later step fails.
func (s *Service) copyAttachments(
	ctx context.Context,
	srcPostID,
	newPostID string,
	opts Options,
	now time.Time,
	keepThread bool,
) ([]*models.PostAttachment, []string, error) {
	if !opts.CopyMedia {
		return nil, nil, nil
	}
	srcAtts, err := s.attachments.ListByPostID(ctx, srcPostID)
	if err != nil {
		return nil, nil, err
	}
	// Refuse rather than write rows that reference never-copied objects.
	if s.store == nil && slices.ContainsFunc(srcAtts, hasBlob) {
		return nil, nil, ErrStorageUnavailable
	}

	var newAtts []*models.PostAttachment
	var copiedKeys []string
	for i, a := range srcAtts {
		attID, err := models.NewID()
		if err != nil {
			return nil, copiedKeys, err
		}
		prefix := "post-attachments/" + newPostID + "/" + attID
		newKey := storage.TenantKey(ctx, prefix+path.Ext(a.S3Key))
		if err := s.copyBlob(ctx, a.S3Key, newKey, &copiedKeys); err != nil {
			return nil, copiedKeys, err
		}
		var newThumb string
		if a.ThumbnailS3Key != "" {
			newThumb = storage.TenantKey(ctx, prefix+".thumb.png")
			if err := s.copyBlob(ctx, a.ThumbnailS3Key, newThumb, &copiedKeys); err != nil {
				return nil, copiedKeys, err
			}
		}

		// Segment membership only survives when the clone stays a thread.
		var segIdx *int
		if keepThread {
			segIdx = a.SegmentIndex
		}
		newAtts = append(newAtts, &models.PostAttachment{
			ID:             attID,
			PostID:         newPostID,
			Position:       i,
			SegmentIndex:   segIdx,
			MimeType:       a.MimeType,
			SizeBytes:      a.SizeBytes,
			Width:          a.Width,
			Height:         a.Height,
			IsAnimated:     a.IsAnimated,
			PageCount:      a.PageCount,
			ChecksumSHA256: a.ChecksumSHA256,
			S3Key:          newKey,
			ThumbnailS3Key: newThumb,
			CreatedBy:      opts.Actor,
			CreatedAt:      now,
		})
	}
	return newAtts, copiedKeys, nil
}

func hasBlob(a models.PostAttachment) bool {
	return a.S3Key != "" || a.ThumbnailS3Key != ""
}

// copyBlob copies srcKey to dstKey and records dstKey in copied. It is a
// no-op when storage is disabled or srcKey is empty.
func (s *Service) copyBlob(ctx context.Context, srcKey, dstKey string, copied *[]string) error {
	if s.store == nil || srcKey == "" {
		return nil
	}
	if err := s.store.Copy(ctx, srcKey, dstKey); err != nil {
		return err
	}
	*copied = append(*copied, dstKey)
	return nil
}

func (s *Service) cleanupKeys(ctx context.Context, keys []string) {
	if s.store == nil {
		return
	}
	for _, k := range keys {
		_ = s.store.Delete(ctx, k)
	}
}

// defaultPostType picks a platform's default post type, preferring
// "text-post" and otherwise the lexicographically first slug.
func defaultPostType(types models.PostTypeMap) string {
	if _, ok := types["text-post"]; ok {
		return "text-post"
	}
	if len(types) == 0 {
		return ""
	}
	return slices.Min(slices.Collect(maps.Keys(types)))
}
