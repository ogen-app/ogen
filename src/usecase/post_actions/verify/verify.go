// Package verify records a manually-published post that Zernio's
// sync-external confirmed: it back-fills the publisher linkage, marks the post
// published, snapshots what went out, and records a first analytics point.
package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/ogen-app/ogen/src/analytics/tracking"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Service confirms external posts. Versions, Analytics and Hub are optional
// (nil skips the content snapshot, the analytics row, and the update event).
type Service struct {
	Posts     repository.PostRepository
	Versions  repository.PostVersionRepository
	Analytics repository.PostAnalyticsRepository
	Hub       eventhub.Hub
}

// Confirm applies a confirmed external post to post and persists it. Only the
// post update can fail; the snapshot and analytics writes after it are
// best-effort because the publish has already committed.
func (s *Service) Confirm(ctx context.Context, post *models.Post, ext *zernio.ExternalPost) error {
	backfill(post, ext)
	if err := s.Posts.Update(ctx, post); err != nil {
		return err
	}
	s.snapshotPublished(ctx, post)
	s.recordAnalytics(ctx, post, ext)
	return nil
}

// backfill links the post to the confirmed external post so per-post
// analytics resolve, and marks it published. Existing linkage is kept, but
// the verified permalink is authoritative and overwrites a user-pasted one.
func backfill(post *models.Post, ext *zernio.ExternalPost) {
	if post.PublisherPostID == "" {
		post.PublisherPostID = ext.PlatformPostID
	}
	if post.Publisher == "" {
		post.Publisher = models.PublisherZernio
	}
	if post.PublishedAt == nil {
		if pa := ext.PublishedAtTime(); pa != nil {
			post.PublishedAt = pa
		}
	}
	if ext.PlatformPostURL != "" {
		post.PublishedURL = ext.PlatformPostURL
	}
	post.Status = models.PostStatusPublished
	post.UpdatedAt = time.Now().UTC()
}

// recordAnalytics refreshes the current-state analytics row with the refresh
// job's dedup discipline and, on a real change, emits the update event so
// open analytics streams refresh — re-verifying an already-tracked post
// doesn't clobber its history.
func (s *Service) recordAnalytics(ctx context.Context, post *models.Post, ext *zernio.ExternalPost) {
	if s.Analytics == nil {
		return
	}
	built := buildCurrent(post, ext)
	prev, _ := s.Analytics.GetByPostID(ctx, post.ID)
	changed, err := tracking.RecordCurrent(ctx, s.Analytics, prev, built, time.Now().UTC())
	if changed && err == nil {
		s.publishUpdated(ctx, built)
	}
}

// ExternalMetrics maps a synced external post's analytics onto the shared
// metrics block.
func ExternalMetrics(a zernio.ExternalPostAnalytics) models.PostAnalyticsMetrics {
	return models.PostAnalyticsMetrics{
		Impressions:    a.Impressions,
		Reach:          a.Reach,
		Likes:          a.Likes,
		Comments:       a.Comments,
		Shares:         a.Shares,
		Saves:          a.Saves,
		Clicks:         a.Clicks,
		Views:          a.Views,
		EngagementRate: a.EngagementRate,
	}
}

// buildCurrent builds the current-state analytics row from a synced external
// post, denormalising the post's display fields like the refresh job. The
// dedup timestamps are stamped by tracking.RecordCurrent.
func buildCurrent(post *models.Post, ext *zernio.ExternalPost) *models.PostAnalytics {
	m := ExternalMetrics(ext.Analytics)
	platformName := ext.Platform
	if post.Platform != nil && post.Platform.Name != "" {
		platformName = post.Platform.Name
	}
	return &models.PostAnalytics{
		PostID: post.ID,
		// The synced external post's id — consistent with the metrics below,
		// not the post's possibly-preexisting linkage.
		PublisherPostID: ext.PlatformPostID,
		Publisher:       models.PublisherZernio,
		Platform:        platformName,
		Title:           post.Title,
		PublishedAt:     post.PublishedAt,
		Impressions:     m.Impressions,
		Reach:           m.Reach,
		Likes:           m.Likes,
		Comments:        m.Comments,
		Shares:          m.Shares,
		Saves:           m.Saves,
		Clicks:          m.Clicks,
		Views:           m.Views,
		EngagementRate:  m.EngagementRate,
		PlatformAnalytics: models.PlatformAnalyticsList{{
			Platform:        ext.Platform,
			PlatformPostID:  ext.PlatformPostID,
			PlatformPostURL: ext.PlatformPostURL,
			SyncStatus:      "synced",
			Analytics:       m,
		}},
		SyncStatus:         "synced",
		MetricsLastUpdated: ext.Analytics.LastUpdatedTime(),
	}
}

// publishUpdated emits post.analytics.updated. No-op without a hub.
func (s *Service) publishUpdated(ctx context.Context, a *models.PostAnalytics) {
	if s.Hub == nil {
		return
	}
	tid, _ := tenantctx.From(ctx)
	_ = s.Hub.Publish(ctx, eventhub.Event{
		Topic:    fmt.Sprintf("entity:post:%s", a.PostID),
		TenantID: tid,
		Type:     "post.analytics.updated",
		Payload: map[string]any{
			"post_id":     a.PostID,
			"sync_status": a.SyncStatus,
			"analytics":   a.Metrics(),
		},
	})
}

// snapshotPublished records a system-authored version of the post's content
// at confirmation, so "what actually went out" is a durable record. It is
// deduped only against a prior "Published" system snapshot of identical
// content: re-verifying adds nothing, but a matching user/assistant edit (or
// the schedule-time "Submitted" snapshot) at the head doesn't suppress it.
// Best-effort: errors are swallowed.
func (s *Service) snapshotPublished(ctx context.Context, post *models.Post) {
	if s.Versions == nil {
		return
	}
	latest, err := s.Versions.GetLatestByPostID(ctx, post.ID)
	if err != nil {
		return
	}
	content := post.SnapshotContent()
	if latest != nil && latest.IsSystemSnapshot() &&
		latest.Note == models.PostVersionNotePublished && latest.Content == content {
		return
	}
	id, err := models.NewID()
	if err != nil {
		return
	}
	_ = s.Versions.CreateNext(ctx, &models.PostVersion{
		ID:      id,
		PostID:  post.ID,
		Content: content,
		Note:    models.PostVersionNotePublished,
		Creator: models.PostVersionCreatorSystem,
	})
}
