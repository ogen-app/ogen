package repository_test

import (
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func queueRow(t *testing.T, db *bun.DB, postID string) (trigger string, due time.Time, claimed bool, ok bool) {
	t.Helper()
	var rows []struct {
		Trigger      string     `bun:"trigger"`
		DueAt        time.Time  `bun:"due_at"`
		ClaimedUntil *time.Time `bun:"claimed_until"`
	}
	if err := db.NewRaw(`SELECT trigger, due_at, claimed_until FROM post_quality_queue WHERE post_id = ?`, postID).
		Scan(t.Context(), &rows); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if len(rows) == 0 {
		return "", time.Time{}, false, false
	}
	return rows[0].Trigger, rows[0].DueAt, rows[0].ClaimedUntil != nil, true
}

func TestPostQualityQueueScheduleReplacesPending(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "qp-1")
	repo := repository.NewPostQualityQueueRepository(db)
	ctx := t.Context()
	base := time.Now().UTC().Truncate(time.Second)

	if err := repo.Schedule(ctx, models.DefaultTenantID, "qp-1", models.QualityTriggerVersion, base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Schedule(ctx, models.DefaultTenantID, "qp-1", models.QualityTriggerReady, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	trigger, due, _, ok := queueRow(t, db, "qp-1")
	if !ok || trigger != string(models.QualityTriggerReady) || !due.Equal(base.Add(time.Minute)) {
		t.Fatalf("row = %q %v %v, want the latest checkpoint", trigger, due, ok)
	}
}

func TestPostQualityQueueClaimFinish(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "qp-due")
	seedPendingPost(t, db, "qp-later")
	repo := repository.NewPostQualityQueueRepository(db)
	ctx := tenantctx.WithSystem(t.Context())
	now := time.Now().UTC().Truncate(time.Second)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(repo.Schedule(ctx, models.DefaultTenantID, "qp-due", models.QualityTriggerReady, now.Add(-time.Second)))
	must(repo.Schedule(ctx, models.DefaultTenantID, "qp-later", models.QualityTriggerVersion, now.Add(time.Hour)))

	claimed, err := repo.Claim(ctx, now, 10, time.Minute)
	must(err)
	if len(claimed) != 1 || claimed[0].PostID != "qp-due" || claimed[0].Attempts != 1 || claimed[0].TenantID != models.DefaultTenantID {
		t.Fatalf("claimed = %+v, want only qp-due on its first attempt", claimed)
	}
	// A held claim is not handed out again.
	again, err := repo.Claim(ctx, now, 10, time.Minute)
	must(err)
	if len(again) != 0 {
		t.Fatalf("a live claim was claimed twice: %+v", again)
	}

	// A checkpoint landing mid-run moves the due time; Finish keeps the row
	// and releases the claim so the new checkpoint runs.
	must(repo.Schedule(ctx, models.DefaultTenantID, "qp-due", models.QualityTriggerVersion, now.Add(10*time.Minute)))
	must(repo.Finish(ctx, claimed[0]))
	trigger, _, isClaimed, ok := queueRow(t, db, "qp-due")
	if !ok || isClaimed || trigger != string(models.QualityTriggerVersion) {
		t.Fatalf("after a mid-run checkpoint the row should stay unclaimed: trigger=%q claimed=%v ok=%v", trigger, isClaimed, ok)
	}

	// Without a newer checkpoint Finish deletes the row.
	later, err := repo.Claim(ctx, now.Add(11*time.Minute), 10, time.Minute)
	must(err)
	if len(later) != 1 || later[0].PostID != "qp-due" {
		t.Fatalf("claimed = %+v, want qp-due", later)
	}
	must(repo.Finish(ctx, later[0]))
	if _, _, _, ok := queueRow(t, db, "qp-due"); ok {
		t.Fatal("finished row should be deleted")
	}
}

func TestPostQualityQueueRetryAndLeaseExpiry(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "qp-retry")
	repo := repository.NewPostQualityQueueRepository(db)
	ctx := tenantctx.WithSystem(t.Context())
	now := time.Now().UTC().Truncate(time.Second)

	if err := repo.Schedule(ctx, models.DefaultTenantID, "qp-retry", models.QualityTriggerReady, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.Claim(ctx, now, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	// An expired lease is claimable again, and counts another attempt.
	reclaimed, err := repo.Claim(ctx, now.Add(2*time.Minute), 10, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaim after lease expiry: %v %+v", err, reclaimed)
	}

	next := now.Add(5 * time.Minute)
	if err := repo.Retry(ctx, reclaimed[0], next); err != nil {
		t.Fatal(err)
	}
	_, due, isClaimed, ok := queueRow(t, db, "qp-retry")
	if !ok || isClaimed || !due.Equal(next) {
		t.Fatalf("retry row: due=%v claimed=%v ok=%v, want due %v unclaimed", due, isClaimed, ok, next)
	}
}

func TestPostEvaluationUpsertCountsRuns(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "qe-1")
	repo := repository.NewPostEvaluationRepository(db)
	ctx := tenantCtx()

	for i := 1; i <= 3; i++ {
		id, err := models.NewID()
		if err != nil {
			t.Fatal(err)
		}
		eval := &models.PostEvaluation{
			ID: id, PostID: "qe-1", PlatformID: "pl", PlatformPostType: "text-post",
			ContentHash: "c", ContextHash: "x", InputHash: "i", Trigger: models.QualityTriggerVersion,
		}
		if err := repo.Upsert(ctx, eval); err != nil {
			t.Fatal(err)
		}
		if eval.ModelRuns != i {
			t.Fatalf("upsert %d: ModelRuns = %d", i, eval.ModelRuns)
		}
	}
	got, err := repo.GetByPostID(ctx, "qe-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.ModelRuns != 3 || got.Trigger != models.QualityTriggerVersion || got.ContentHash != "c" || got.ContextHash != "x" {
		t.Fatalf("stored = %+v", got)
	}
}

// OverallByPost reads the scores of the evaluated posts named; the rest are
// absent.
func TestPostEvaluationOverallByPost(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "qs-1")
	seedPendingPost(t, db, "qs-2")
	repo := repository.NewPostEvaluationRepository(db)
	ctx := tenantCtx()
	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, &models.PostEvaluation{ID: id, PostID: "qs-1", PlatformID: "pl", PlatformPostType: "text-post",
		OverallPct: 42.5, ContentHash: "c", ContextHash: "x", InputHash: "i", Trigger: models.QualityTriggerManual}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.OverallByPost(ctx, []string{"qs-1", "qs-2", "missing"})
	if err != nil || len(got) != 1 || got["qs-1"] != 42.5 {
		t.Fatalf("scores = %v, %v", got, err)
	}
	if got, err := repo.OverallByPost(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no ids = %v, %v", got, err)
	}
}
