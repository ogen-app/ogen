package repository_test

import (
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func getEmailLog(t *testing.T, db *bun.DB, id string) models.EmailLog {
	t.Helper()
	var l models.EmailLog
	if err := db.NewSelect().Model(&l).Where("id = ?", id).Scan(t.Context()); err != nil {
		t.Fatalf("get email_log: %v", err)
	}
	return l
}

func emailLogIDs(rows []models.EmailLog) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

// TestEmailEventRecordAndRollup covers the CON-298 ingestion invariants: the
// rollup advances with the lifecycle, redelivery (same svix id) is a no-op, and
// an out-of-order (earlier) open still updates first_opened_at without moving
// last_event backwards.
func TestEmailEventRecordAndRollup(t *testing.T) {
	db := openMigratedDB(t)
	logs := repository.NewEmailLogRepository(db)
	events := repository.NewEmailEventRepository(db)
	ctx := t.Context()

	logID := mustID(t)
	if err := logs.Insert(ctx, &models.EmailLog{
		ID: logID, TemplateID: "welcome", Kind: models.EmailKindTransactional,
		ToEmail: "a@b.com", Status: models.EmailLogSent, Provider: models.ProviderResend,
		ProviderMessageID: "msg_roll",
	}); err != nil {
		t.Fatalf("insert log: %v", err)
	}

	t0 := time.Now().UTC().Truncate(time.Second)
	rec := func(svix string, typ models.EmailEventType, at time.Time) {
		t.Helper()
		if err := events.Record(ctx, &models.EmailEvent{
			ID: mustID(t), EmailLogID: logID, ProviderMessageID: "msg_roll",
			Type: typ, OccurredAt: at, SvixID: svix,
		}); err != nil {
			t.Fatalf("record %s: %v", typ, err)
		}
	}

	rec("sv_del", models.EmailEventDelivered, t0)
	rec("sv_open1", models.EmailEventOpened, t0.Add(time.Minute))
	rec("sv_click", models.EmailEventClicked, t0.Add(2*time.Minute))

	got := getEmailLog(t, db, logID)
	if got.Status != models.EmailLogClicked {
		t.Fatalf("status: got %q want clicked", got.Status)
	}
	if got.OpensCount != 1 || got.ClicksCount != 1 {
		t.Fatalf("counts: opens=%d clicks=%d want 1/1", got.OpensCount, got.ClicksCount)
	}
	if got.DeliveredAt.IsZero() || got.FirstOpenedAt.IsZero() {
		t.Fatalf("first-occurrence ts not set: delivered=%v firstOpened=%v", got.DeliveredAt, got.FirstOpenedAt)
	}
	if got.LastEvent != string(models.EmailEventClicked) {
		t.Fatalf("last_event: got %q want clicked", got.LastEvent)
	}

	// Redelivery of the same open (same svix id) must not double-count.
	rec("sv_open1", models.EmailEventOpened, t0.Add(time.Minute))
	if g := getEmailLog(t, db, logID); g.OpensCount != 1 {
		t.Fatalf("redelivery double-counted opens: %d", g.OpensCount)
	}

	// A distinct open that occurred EARLIER updates first_opened_at, counts, but
	// leaves last_event on the newest event (the click).
	earlier := t0.Add(-30 * time.Second)
	rec("sv_open2", models.EmailEventOpened, earlier)
	got = getEmailLog(t, db, logID)
	if got.OpensCount != 2 {
		t.Fatalf("second open not counted: %d", got.OpensCount)
	}
	if !got.FirstOpenedAt.Equal(earlier) {
		t.Fatalf("first_opened_at not the earliest: got %v want %v", got.FirstOpenedAt, earlier)
	}
	if got.LastEvent != string(models.EmailEventClicked) {
		t.Fatalf("last_event regressed to %q", got.LastEvent)
	}
}

// TestEmailEventNoStatusRegression asserts a terminal-negative state (complaint)
// is never overwritten by a late positive event, while the positive event's
// timestamps/counts are still recorded (CON-298).
func TestEmailEventNoStatusRegression(t *testing.T) {
	db := openMigratedDB(t)
	logs := repository.NewEmailLogRepository(db)
	events := repository.NewEmailEventRepository(db)
	ctx := t.Context()

	logID := mustID(t)
	if err := logs.Insert(ctx, &models.EmailLog{
		ID: logID, TemplateID: "welcome", Kind: models.EmailKindMarketing,
		ToEmail: "a@b.com", Status: models.EmailLogSent, Provider: models.ProviderResend,
		ProviderMessageID: "msg_reg",
	}); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	t0 := time.Now().UTC().Truncate(time.Second)

	if err := events.Record(ctx, &models.EmailEvent{ID: mustID(t), EmailLogID: logID, Type: models.EmailEventComplained, OccurredAt: t0.Add(time.Minute), SvixID: "sv_c"}); err != nil {
		t.Fatalf("record complaint: %v", err)
	}
	// A late 'delivered' (earlier occurred_at) must not regress status.
	if err := events.Record(ctx, &models.EmailEvent{ID: mustID(t), EmailLogID: logID, Type: models.EmailEventDelivered, OccurredAt: t0, SvixID: "sv_d"}); err != nil {
		t.Fatalf("record delivered: %v", err)
	}

	got := getEmailLog(t, db, logID)
	if got.Status != models.EmailLogComplained {
		t.Fatalf("status regressed: got %q want complained", got.Status)
	}
	if got.DeliveredAt.IsZero() {
		t.Fatal("delivered_at should be recorded even under a complaint")
	}
	if got.LastEvent != string(models.EmailEventComplained) {
		t.Fatalf("last_event: got %q want complained (newest)", got.LastEvent)
	}

	// Timeline is oldest-first.
	tl, err := events.ListByEmailLogID(ctx, logID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(tl) != 2 || tl[0].Type != models.EmailEventDelivered || tl[1].Type != models.EmailEventComplained {
		t.Fatalf("timeline order: %+v", tl)
	}
}

// TestEmailLogListByTenant covers tenant-scoped keyset pagination + filters and
// NULL-tenant exclusion (CON-298). FKs are disabled by openMigratedDB, so raw
// tenant_id strings need no tenants row.
func TestEmailLogListByTenant(t *testing.T) {
	db := openMigratedDB(t)
	logs := repository.NewEmailLogRepository(db)
	ctx := t.Context()

	base := time.Now().UTC().Truncate(time.Second)
	mk := func(id, tenant string, kind models.EmailKind, st models.EmailLogStatus, to string, at time.Time) {
		t.Helper()
		if err := logs.Insert(ctx, &models.EmailLog{
			ID: id, TenantID: tenant, TemplateID: "welcome", Kind: kind,
			ToEmail: to, Status: st, Provider: models.ProviderResend, CreatedAt: at,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mk("e1", "tn-e", models.EmailKindTransactional, models.EmailLogSent, "alice@x.com", base.Add(time.Minute))
	mk("e2", "tn-e", models.EmailKindMarketing, models.EmailLogDelivered, "bob@x.com", base.Add(2*time.Minute))
	mk("e3", "tn-e", models.EmailKindTransactional, models.EmailLogOpened, "carol@x.com", base.Add(3*time.Minute))
	mk("e4", "", models.EmailKindTransactional, models.EmailLogSent, "sys@x.com", base.Add(4*time.Minute)) // system mail

	// Page 1 (size 2): newest-first → e3, e2.
	page1, err := logs.ListByTenant(ctx, repository.EmailListFilter{TenantID: "tn-e", Limit: 2})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if got := emailLogIDs(page1); len(got) != 2 || got[0] != "e3" || got[1] != "e2" {
		t.Fatalf("page1 order: %v", got)
	}
	// Page 2 from the keyset cursor → e1 only (e4 excluded: null tenant).
	last := page1[1]
	page2, err := logs.ListByTenant(ctx, repository.EmailListFilter{TenantID: "tn-e", Limit: 2, CursorCreatedAt: last.CreatedAt, CursorID: last.ID})
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if got := emailLogIDs(page2); len(got) != 1 || got[0] != "e1" {
		t.Fatalf("page2: %v", got)
	}

	// kind filter.
	if got, _ := logs.ListByTenant(ctx, repository.EmailListFilter{TenantID: "tn-e", Kind: models.EmailKindMarketing, Limit: 10}); len(got) != 1 || got[0].ID != "e2" {
		t.Fatalf("kind filter: %v", emailLogIDs(got))
	}
	// status any-of filter.
	if got, _ := logs.ListByTenant(ctx, repository.EmailListFilter{TenantID: "tn-e", Statuses: []models.EmailLogStatus{models.EmailLogOpened, models.EmailLogSent}, Limit: 10}); len(got) != 2 {
		t.Fatalf("status filter: %v", emailLogIDs(got))
	}
	// case-insensitive recipient substring.
	if got, _ := logs.ListByTenant(ctx, repository.EmailListFilter{TenantID: "tn-e", Recipient: "BOB", Limit: 10}); len(got) != 1 || got[0].ID != "e2" {
		t.Fatalf("recipient filter: %v", emailLogIDs(got))
	}

	// Tenant scoping on the point read.
	if got, _ := logs.GetByIDForTenant(ctx, "tn-e", "e1"); got == nil || got.ID != "e1" {
		t.Fatalf("get in-tenant: %+v", got)
	}
	if got, _ := logs.GetByIDForTenant(ctx, "other", "e1"); got != nil {
		t.Fatal("cross-tenant read must return nil")
	}
}
