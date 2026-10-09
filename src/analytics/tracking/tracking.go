// Package tracking maintains a post's current-state analytics row with
// change-deduplicated trend history: every refresh bumps the current row's
// freshness, but a history point is appended only when the metrics moved.
package tracking

import (
	"context"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Stamp sets built's dedup timestamps relative to the stored row prev (nil on
// a first sighting) and reports whether the metrics changed: first_seen_at is
// preserved, last_checked_at is always now, and last_changed_at moves only on
// a change.
func Stamp(prev, built *models.PostAnalytics, now time.Time) bool {
	changed := prev == nil || prev.MetricsKey() != built.MetricsKey()
	built.LastCheckedAt = now
	switch {
	case prev == nil:
		built.FirstSeenAt = now
		built.LastChangedAt = now
	case changed:
		built.FirstSeenAt = prev.FirstSeenAt
		built.LastChangedAt = now
	default:
		built.FirstSeenAt = prev.FirstSeenAt
		built.LastChangedAt = prev.LastChangedAt
	}
	return changed
}

// sameStored reports whether built would store exactly what prev holds, apart
// from the dedup timestamps. Stamp has already compared the metrics.
func sameStored(prev, built *models.PostAnalytics) bool {
	if prev == nil {
		return false
	}
	if prev.Publisher != built.Publisher || prev.PublisherPostID != built.PublisherPostID ||
		prev.Platform != built.Platform || prev.Title != built.Title ||
		!equalTime(prev.PublishedAt, built.PublishedAt) || !equalTime(prev.MetricsLastUpdated, built.MetricsLastUpdated) {
		return false
	}
	// Compare the payloads as they are stored, so a nil and an empty list match.
	pv, perr := prev.PlatformAnalytics.Value()
	bv, berr := built.PlatformAnalytics.Value()
	return perr == nil && berr == nil && pv == bv
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// RecordCurrent stamps built against prev and writes it. On a change the
// current row and its trend snapshot are written atomically, so a snapshot
// failure can't leave the row advanced without its history point (dedup
// would then hide the change forever); otherwise only the current row is
// bumped. It reports whether the metrics changed.
func RecordCurrent(ctx context.Context, repo repository.PostAnalyticsRepository, prev, built *models.PostAnalytics, now time.Time) (bool, error) {
	if !Stamp(prev, built, now) {
		// Most refreshes of a settled post change nothing but the check time;
		// moving that alone avoids rewriting the row and its jsonb payload.
		if sameStored(prev, built) {
			return false, repo.TouchChecked(ctx, built.PostID, now)
		}
		return false, repo.Upsert(ctx, built)
	}
	id, err := models.NewID()
	if err != nil {
		return true, err
	}
	return true, repo.UpsertWithSnapshot(ctx, built, built.NewSnapshot(id, now))
}
