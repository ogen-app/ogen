package tracking

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type fakeRepo struct {
	repository.PostAnalyticsRepository
	upserts, snapshots int
}

func (f *fakeRepo) Upsert(context.Context, *models.PostAnalytics) error {
	f.upserts++
	return nil
}

func (f *fakeRepo) UpsertWithSnapshot(context.Context, *models.PostAnalytics, *models.PostAnalyticsSnapshot) error {
	f.snapshots++
	return nil
}

func TestStamp(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0.Add(time.Hour)

	first := &models.PostAnalytics{Likes: 1}
	if !Stamp(nil, first, now) || !first.FirstSeenAt.Equal(now) || !first.LastChangedAt.Equal(now) || !first.LastCheckedAt.Equal(now) {
		t.Fatalf("first sighting: %+v", first)
	}

	prev := &models.PostAnalytics{Likes: 1, FirstSeenAt: t0, LastChangedAt: t0}
	same := &models.PostAnalytics{Likes: 1}
	if Stamp(prev, same, now) || !same.FirstSeenAt.Equal(t0) || !same.LastChangedAt.Equal(t0) || !same.LastCheckedAt.Equal(now) {
		t.Fatalf("unchanged: %+v", same)
	}

	moved := &models.PostAnalytics{Likes: 2}
	if !Stamp(prev, moved, now) || !moved.FirstSeenAt.Equal(t0) || !moved.LastChangedAt.Equal(now) {
		t.Fatalf("changed: %+v", moved)
	}
}

func TestRecordCurrent(t *testing.T) {
	now := time.Now().UTC()
	repo := &fakeRepo{}
	prev := &models.PostAnalytics{Likes: 1}

	changed, err := RecordCurrent(t.Context(), repo, prev, &models.PostAnalytics{Likes: 1}, now)
	if err != nil || changed || repo.upserts != 1 || repo.snapshots != 0 {
		t.Fatalf("unchanged: changed=%v err=%v repo=%+v", changed, err, repo)
	}
	changed, err = RecordCurrent(t.Context(), repo, prev, &models.PostAnalytics{Likes: 5}, now)
	if err != nil || !changed || repo.upserts != 1 || repo.snapshots != 1 {
		t.Fatalf("changed: changed=%v err=%v repo=%+v", changed, err, repo)
	}
}
