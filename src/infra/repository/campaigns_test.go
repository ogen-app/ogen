package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// seedCampaign inserts one active campaign and returns its id. FK enforcement is
// disabled by openMigratedDB, so synthetic campaign_type_id / created_by refs
// need no backing rows.
func seedCampaign(t *testing.T, repo repository.CampaignRepository, ctx context.Context, name string) string {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	now := time.Now().UTC()
	c := &models.Campaign{
		ID:              id,
		Name:            name,
		Description:     "desc",
		TargetPersona:   "persona",
		KeyMessages:     "messages",
		ToneGuidelines:  "tone",
		AssetIDs:        models.StringSlice{},
		TargetPlatforms: models.CampaignPlatforms{},
		CampaignTypeID:  "ctype-1",
		Language:        "en",
		PublishingTime:  "09:00",
		PublishingDays:  models.StringSlice{},
		SpreadMinutes:   15,
		GoalCadence:     "month",
		Status:          models.StatusActive,
		Currency:        "USD",
		TagIDs:          models.StringSlice{},
		CreatedBy:       "user-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	return id
}

// readArchivedAt fetches archived_at straight from the row, bypassing the
// repo's hydration path (which needs sub-repos this test doesn't wire).
func readArchivedAt(t *testing.T, db *bun.DB, ctx context.Context, id string) *time.Time {
	t.Helper()
	c := new(models.Campaign)
	if err := db.NewSelect().Model(c).Where("c.id = ?", id).Scan(ctx); err != nil {
		t.Fatalf("read archived_at: %v", err)
	}
	return c.ArchivedAt
}

// TestArchivePreservesTimestampOnReArchive is the regression for the re-archive
// overwrite: archiving an already archived campaign must keep the original
// archived_at rather than re-stamping it to the current time.
func TestArchivePreservesTimestampOnReArchive(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewCampaignRepository(db, nil, nil, nil)

	id := seedCampaign(t, repo, ctx, "Re-archive Me")

	ok, err := repo.Archive(ctx, id)
	if err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if !ok {
		t.Fatal("first archive matched no row")
	}
	first := readArchivedAt(t, db, ctx, id)
	if first == nil {
		t.Fatal("archived_at not set after first archive")
	}

	// A gap so a spurious re-stamp would be visibly different (timestamptz keeps
	// microsecond precision).
	time.Sleep(10 * time.Millisecond)

	ok, err = repo.Archive(ctx, id)
	if err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if !ok {
		t.Fatal("re-archive of a live campaign must still report a matched row")
	}
	second := readArchivedAt(t, db, ctx, id)
	if second == nil {
		t.Fatal("archived_at cleared by re-archive")
	}
	if !second.Equal(*first) {
		t.Fatalf("re-archive moved archived_at: first=%s second=%s", first, second)
	}

	// Unarchive still clears the timestamp; a subsequent archive stamps fresh.
	if ok, err := repo.Unarchive(ctx, id); err != nil || !ok {
		t.Fatalf("unarchive: ok=%v err=%v", ok, err)
	}
	if got := readArchivedAt(t, db, ctx, id); got != nil {
		t.Fatalf("unarchive did not clear archived_at: %s", got)
	}
}

// SetDates writes only while the dates are still the ones the caller read.
func TestSetDates_OnlyWhileUnchanged(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewCampaignRepository(db, nil, nil, nil)
	id := seedCampaign(t, repo, ctx, "Dated")

	readDates := func() (start, end *time.Time) {
		t.Helper()
		c := new(models.Campaign)
		if err := db.NewSelect().Model(c).Where("c.id = ?", id).Scan(ctx); err != nil {
			t.Fatalf("read dates: %v", err)
		}
		return c.StartDate, c.EndDate
	}
	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jan31 := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	mar31 := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	// From no dates to a range.
	if ok, err := repo.SetDates(ctx, id, nil, nil, &jan1, &jan31); err != nil || !ok {
		t.Fatalf("set from empty: ok=%v err=%v", ok, err)
	}
	// Stale expectations write nothing.
	if ok, err := repo.SetDates(ctx, id, nil, nil, &jan1, &mar31); err != nil || ok {
		t.Fatalf("stale set: ok=%v err=%v, want no write", ok, err)
	}
	if _, end := readDates(); !end.Equal(jan31) {
		t.Fatalf("end = %v after a stale set, want %v", end, jan31)
	}
	// Current expectations write.
	start, end := readDates()
	if ok, err := repo.SetDates(ctx, id, start, end, &jan1, &mar31); err != nil || !ok {
		t.Fatalf("current set: ok=%v err=%v", ok, err)
	}
	if _, end := readDates(); !end.Equal(mar31) {
		t.Fatalf("end = %v, want %v", end, mar31)
	}
}
