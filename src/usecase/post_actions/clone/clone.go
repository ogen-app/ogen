// Package clone implements the shared "clone a Post" operation
// (CON-59). It is the single source of truth used by both the REST
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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
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
	TriggerAPI       = "api"
	TriggerAssistant = "assistant"
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
	// platform default (AC #3 — the caller should tell the user).
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

// Clone duplicates the post identified by sourceID per opts and returns
// the new draft. The DB writes (post, attachment rows, v1 version,
// audit entry) commit in one transaction; storage copies happen before
// the transaction and are cleaned up if it fails.
func (s *Service) Clone(ctx context.Context, sourceID string, opts Options) (*Result, error) {
	src, err := s.posts.GetByID(ctx, sourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSourceNotFound
		}
		return nil, err
	}

	// Resolve target platform + post type.
	targetPlatformID := opts.TargetPlatformID
	targetPlatformID = cmp.Or(targetPlatformID, src.PlatformID)
	targetPostType := opts.TargetPostType
	if targetPostType == "" {
		targetPostType = src.PlatformPostType
	}
	crossPlatform := targetPlatformID != "" && targetPlatformID != src.PlatformID

	var targetPlatform *models.Platform
	fellBack := false
	if targetPlatformID != "" {
		p, err := s.platforms.GetByID(ctx, targetPlatformID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: %s", ErrInvalidPlatform, targetPlatformID)
			}
			return nil, err
		}
		targetPlatform = p
		if _, ok := p.PostTypes[targetPostType]; targetPostType == "" || !ok {
			def := defaultPostType(p.PostTypes)
			if def == "" {
				return nil, fmt.Errorf("%w: %s has no post types", ErrInvalidPlatform, targetPlatformID)
			}
			// Only a fallback worth reporting if the caller asked for a
			// specific (non-empty) type that didn't fit the platform.
			fellBack = targetPostType != "" && targetPostType != def
			targetPostType = def
		}
	} else {
		// Platform-less draft clone: no post type either.
		targetPostType = ""
	}

	// Title convention.
	title := src.Title
	switch {
	case opts.TitleOverride != nil:
		title = *opts.TitleOverride
	case crossPlatform && targetPlatform != nil:
		title = src.Title + " (" + targetPlatform.Name + ")"
	}

	// Content.
	content := src.Content
	if opts.ContentOverride != nil {
		content = *opts.ContentOverride
	}
	adapted := content != src.Content

	newID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	clone := &models.Post{
		ID:               newID,
		CampaignID:       src.CampaignID,
		PlatformID:       targetPlatformID,
		PlatformPostType: targetPostType,
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
	// CON-284: keep the thread's ordered segments only when the clone remains a
	// thread and the content wasn't adapted (an adapted body would diverge from
	// the stored segments). A retarget to a single-message type demotes the
	// clone — segments stay empty, matching the PUT demotion path.
	keepThread := targetPostType == models.PostTypeThread && !adapted
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

	// Deep-copy attachments in object storage before the DB transaction
	// so the rows reference keys that already exist. copiedKeys lets us
	// roll the storage side back if the transaction fails.
	newAtts, copiedKeys, err := s.copyAttachments(ctx, src.ID, newID, opts, now, keepThread)
	if err != nil {
		s.cleanupKeys(context.Background(), copiedKeys)
		return nil, err
	}

	versionID, err := models.NewID()
	if err != nil {
		s.cleanupKeys(context.Background(), copiedKeys)
		return nil, err
	}
	// post_versions.creator is a role enum ('user' | 'assistant'), not a
	// user id — an assistant-driven clone (which may adapt content) is
	// "assistant"; anything else is "user".
	versionCreator := "user"
	if opts.Trigger == TriggerAssistant {
		versionCreator = "assistant"
	}
	version := &models.PostVersion{
		ID:            versionID,
		PostID:        newID,
		VersionNumber: 1,
		Content:       content,
		Note:          fmt.Sprintf("Cloned from #%s", src.ID),
		Creator:       versionCreator,
	}

	logEntry, err := buildLogEntry(newID, src.ID, targetPlatformID, opts.Actor, opts.Trigger, adapted)
	if err != nil {
		s.cleanupKeys(context.Background(), copiedKeys)
		return nil, err
	}

	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(clone).Exec(ctx); err != nil {
			return err
		}
		for _, a := range newAtts {
			if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.NewInsert().Model(version).Exec(ctx); err != nil {
			return err
		}
		if s.logs != nil {
			if err := s.logs.AppendTx(ctx, tx, logEntry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.cleanupKeys(context.Background(), copiedKeys)
		return nil, err
	}

	s.publishCloned(src.ID, newID, opts.Actor, adapted)

	return &Result{
		Post:             clone,
		Adapted:          adapted,
		PostTypeFellBack: fellBack,
		ResolvedPostType: targetPostType,
	}, nil
}

// copyAttachments duplicates the source post's attachments to newPostID,
// copying each blob (and thumbnail) to a fresh key. Returns the new
// rows (to be inserted in the caller's transaction) and the list of
// keys written (for rollback on a later failure).
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

	// Without object storage we cannot copy the blobs, so refuse rather
	// than write attachment rows that reference never-copied objects.
	if s.store == nil {
		for _, a := range srcAtts {
			if a.S3Key != "" || a.ThumbnailS3Key != "" {
				return nil, nil, ErrStorageUnavailable
			}
		}
	}

	var newAtts []*models.PostAttachment
	var copiedKeys []string
	for i, a := range srcAtts {
		attID, err := models.NewID()
		if err != nil {
			return nil, copiedKeys, err
		}

		newKey := storage.TenantKey(ctx, "post-attachments/"+newPostID+"/"+attID+path.Ext(a.S3Key))
		if s.store != nil && a.S3Key != "" {
			if err := s.store.Copy(ctx, a.S3Key, newKey); err != nil {
				return nil, copiedKeys, err
			}
			copiedKeys = append(copiedKeys, newKey)
		}

		var newThumb string
		if a.ThumbnailS3Key != "" {
			newThumb = storage.TenantKey(ctx, "post-attachments/"+newPostID+"/"+attID+".thumb.png")
			if s.store != nil {
				if err := s.store.Copy(ctx, a.ThumbnailS3Key, newThumb); err != nil {
					return nil, copiedKeys, err
				}
				copiedKeys = append(copiedKeys, newThumb)
			}
		}

		// CON-284: carry which thread segment the media belonged to only when
		// the clone stays a thread; otherwise it becomes an ordinary (NULL)
		// attachment.
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

func (s *Service) cleanupKeys(ctx context.Context, keys []string) {
	if s.store == nil {
		return
	}
	for _, k := range keys {
		_ = s.store.Delete(ctx, k)
	}
}

func (s *Service) publishCloned(srcID, newID, actor string, adapted bool) {
	if s.hub == nil {
		return
	}
	evID, err := models.NewID()
	if err != nil {
		return
	}
	_ = s.hub.Publish(context.Background(), eventhub.Event{
		ID:     evID,
		Topic:  "entity:post:" + newID,
		// Dotted bus wire type (CON-285). Distinct from the post_logs.event_type
		// and tenant_activity_events taxonomy constants, which stay "post_cloned"
		// (persisted history + stability contract — do not rename those).
		Type:   "post.cloned",
		UserID: actor,
		Payload: map[string]any{
			"sourcePostId": srcID,
			"newPostId":    newID,
			"adapted":      adapted,
		},
	})
}

func buildLogEntry(newID, srcID, targetPlatform, actor, trigger string, adapted bool) (*models.PostLog, error) {
	logID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{
		"source_post_id":  srcID,
		"new_post_id":     newID,
		"target_platform": targetPlatform,
		"adapted":         adapted,
		"trigger":         trigger,
	})
	return &models.PostLog{
		ID:        logID,
		PostID:    newID,
		EventType: models.PostLogEventPostCloned,
		Actor:     actor,
		Summary:   "post cloned from #" + srcID,
		Payload:   logs.SanitizeAndCap(string(payload)),
	}, nil
}

// defaultPostType picks a platform's default post type, preferring
// "text-post" and otherwise the lexicographically first slug.
func defaultPostType(types models.PostTypeMap) string {
	if _, ok := types["text-post"]; ok {
		return "text-post"
	}
	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(keys) > 0 {
		return keys[0]
	}
	return ""
}
