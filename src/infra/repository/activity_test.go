package repository_test

import (
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// backfillCutoff is a far-future upper bound for the tests: every fixture row
// predates it, so the backfill's [watermark, before) window includes them all —
// preserving the pre-cutoff behaviour. TestBackfillPostLogsToActivity_ExcludesAtOrAfterCutoff
// exercises the bound itself.
var backfillCutoff = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

func seedPostLog(t *testing.T, db *bun.DB, id, postID string, evt models.PostLogEventType, actor string, from, to *models.PostStatus, at time.Time, summary string) {
	t.Helper()
	entry := &models.PostLog{
		ID:             id,
		PostID:         postID,
		EventTimestamp: at,
		EventType:      evt,
		Actor:          actor,
		FromStatus:     from,
		ToStatus:       to,
		Payload:        "{}",
		Summary:        summary,
	}
	if _, err := db.NewInsert().Model(entry).Exec(tenantCtx()); err != nil {
		t.Fatalf("seed post_log %s: %v", id, err)
	}
}

// TestBackfillPostLogsToActivity_WatermarkTie proves the watermark window is
// `>= watermark` (not `>`): a new meaningful post_log added at the EXACT
// watermark timestamp is still migrated on a re-run, while the row that set the
// watermark is not duplicated (seen-map dedup).
func TestBackfillPostLogsToActivity_WatermarkTie(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()

	draft := models.PostStatusDraft
	ready := models.PostStatusReadyForPublish
	t0 := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	watermarkTS := t0.Add(time.Minute)

	seedPostLog(t, db, "w1", "post-1", models.PostLogEventStateTransition, "user-1", &draft, &ready, t0, "")
	seedPostLog(t, db, "w2", "post-1", models.PostLogEventPostCloned, "user-1", nil, nil, watermarkTS, "") // sets the watermark

	if n, err := repository.BackfillPostLogsToActivity(ctx, db, db, backfillCutoff); err != nil || n != 2 {
		t.Fatalf("first run: n=%d err=%v want 2", n, err)
	}

	// A NEW meaningful row at the exact watermark timestamp (a tie).
	seedPostLog(t, db, "w3", "post-1", models.PostLogEventPostRestored, "user-1", nil, nil, watermarkTS, "")

	n, err := repository.BackfillPostLogsToActivity(ctx, db, db, backfillCutoff)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n != 1 {
		t.Fatalf("tie row migrated %d, want 1 (>= watermark + seen-map dedup)", n)
	}
	count, err := db.NewSelect().Model((*models.ActivityEvent)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("rows after tie = %d, want 3 (w2 not duplicated, w3 added)", count)
	}
}

// TestBackfillPostLogsToActivity_ExcludesAtOrAfterCutoff proves the `before`
// upper bound is exclusive: a row at the cutoff is skipped, one just before it is
// migrated. This is what keeps the boot-time background backfill (CON-301) from
// racing the live post-transition path — which records its own activity event —
// into a duplicate for the same post_log.
func TestBackfillPostLogsToActivity_ExcludesAtOrAfterCutoff(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()

	cutoff := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedPostLog(t, db, "before", "post-1", models.PostLogEventPostCloned, "user-1", nil, nil, cutoff.Add(-time.Second), "")
	seedPostLog(t, db, "at", "post-1", models.PostLogEventPostCloned, "user-1", nil, nil, cutoff, "")
	seedPostLog(t, db, "after", "post-1", models.PostLogEventPostCloned, "user-1", nil, nil, cutoff.Add(time.Second), "")

	n, err := repository.BackfillPostLogsToActivity(ctx, db, db, cutoff)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated %d, want 1 (only the row before the cutoff)", n)
	}
	count, err := db.NewSelect().Model((*models.ActivityEvent)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("activity rows = %d, want 1 (at-cutoff and after-cutoff excluded)", count)
	}
}

func TestBackfillPostLogsToActivity(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()

	draft := models.PostStatusDraft
	ready := models.PostStatusReadyForPublish
	base := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	// Two meaningful events (one user, one system) + one operational-noise event
	// that must be skipped.
	seedPostLog(t, db, "l1", "post-1", models.PostLogEventStateTransition, "user-7", &draft, &ready, base, "moved to ready")
	seedPostLog(t, db, "l2", "post-1", models.PostLogEventZernioSubmit, models.ActorSystem, &ready, &ready, base.Add(time.Minute), "submitted")
	seedPostLog(t, db, "l3", "post-1", models.PostLogEventTaskEnqueued, models.ActorSystem, nil, nil, base.Add(2*time.Minute), "enqueued")

	n, err := repository.BackfillPostLogsToActivity(ctx, db, db, backfillCutoff)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("migrated %d want 2 (task_enqueued must be skipped)", n)
	}

	var events []models.ActivityEvent
	if err := db.NewSelect().Model(&events).OrderExpr("ae.occurred_at").Scan(ctx); err != nil {
		t.Fatalf("scan tenant_activity_events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("activity rows = %d want 2", len(events))
	}

	// state_transition → post/post_state_transition, attributed to the user.
	st := events[0]
	if st.Category != "post" || st.Type != "post_state_transition" {
		t.Errorf("state_transition mapped wrong: %s/%s", st.Category, st.Type)
	}
	if st.EntityType != "post" || st.EntityID != "post-1" {
		t.Errorf("entity wrong: %s/%s", st.EntityType, st.EntityID)
	}
	if st.UserID != "user-7" || st.Source != "api" {
		t.Errorf("actor mapping wrong: user=%q source=%q", st.UserID, st.Source)
	}
	if st.Status != "draft->ready_for_publish" {
		t.Errorf("status transition wrong: %q", st.Status)
	}
	if !st.OccurredAt.Equal(base) {
		t.Errorf("occurred_at = %v want %v", st.OccurredAt, base)
	}
	if st.Payload["post_log_event_type"] != "state_transition" {
		t.Errorf("payload provenance missing: %+v", st.Payload)
	}
	if len(st.Tags) != 1 || st.Tags[0] != "post_logs_backfill" {
		t.Errorf("backfill tag missing: %+v", st.Tags)
	}

	// zernio_submit → publish/publish_submitted, system actor ⇒ job source, no user.
	sub := events[1]
	if sub.Category != "publish" || sub.Type != "publish_submitted" {
		t.Errorf("zernio_submit mapped wrong: %s/%s", sub.Category, sub.Type)
	}
	if sub.UserID != "" || sub.Source != "job" {
		t.Errorf("system actor should have no user + job source: user=%q source=%q", sub.UserID, sub.Source)
	}

	// Idempotent: a second run inserts nothing.
	n2, err := repository.BackfillPostLogsToActivity(ctx, db, db, backfillCutoff)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second run migrated %d, want 0 (idempotent)", n2)
	}
	count, err := db.NewSelect().Model((*models.ActivityEvent)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("activity rows after re-run = %d want 2", count)
	}

	// A new post_log added after the first migration IS picked up on the next run.
	seedPostLog(t, db, "l4", "post-1", models.PostLogEventPostCloned, "user-7", nil, nil, base.Add(time.Hour), "cloned")
	n3, err := repository.BackfillPostLogsToActivity(ctx, db, db, backfillCutoff)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if n3 != 1 {
		t.Errorf("third run migrated %d, want 1 (the new post_cloned)", n3)
	}
}
