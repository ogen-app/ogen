package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// openNotifDB returns a fresh, fully-migrated DB with FK enforcement bypassed
// so a test can insert notifications with synthetic tenant_id/user_id without
// seeding the tenants/users graph (CON-242). Mirrors openMigratedDB but without
// the analytics migration set — notifications live in the main DB.
func openNotifDB(t *testing.T) *bun.DB {
	t.Helper()
	db := pgtest.MustDB()
	db.DB.SetMaxOpenConns(1)
	db.DB.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tctx(tenant string) context.Context {
	return tenantctx.With(context.Background(), tenant)
}

func newNotif(id, user, typ string) *models.Notification {
	return &models.Notification{
		ID:     id,
		UserID: user,
		Level:  models.NotificationLevelInfo,
		Type:   typ,
		Title:  "t-" + id,
	}
}

func TestNotifications_InsertPopulatesSeqAndCreatedAt(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	n1 := newNotif("n1", "u1", "post.published")
	ok, err := repo.Insert(ctx, n1)
	if err != nil || !ok {
		t.Fatalf("insert n1: ok=%v err=%v", ok, err)
	}
	if n1.Seq == 0 {
		t.Fatalf("seq not populated from RETURNING")
	}
	if n1.CreatedAt.IsZero() {
		t.Fatalf("created_at not populated from RETURNING")
	}

	n2 := newNotif("n2", "u1", "post.published")
	if _, err := repo.Insert(ctx, n2); err != nil {
		t.Fatalf("insert n2: %v", err)
	}
	if n2.Seq <= n1.Seq {
		t.Fatalf("seq not monotonic: n1=%d n2=%d", n1.Seq, n2.Seq)
	}
}

func TestNotifications_DedupeCollapsesWhileUnread(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	first := &models.Notification{ID: "d1", UserID: "u1", Level: models.NotificationLevelWarning, Type: "connection.expiring_soon", Title: "a", DedupeKey: "conn:acc:soon"}
	ok, err := repo.Insert(ctx, first)
	if err != nil || !ok {
		t.Fatalf("first insert: ok=%v err=%v", ok, err)
	}

	// Same (user, dedupe_key) while unread → collapsed (no row).
	dup := &models.Notification{ID: "d2", UserID: "u1", Level: models.NotificationLevelWarning, Type: "connection.expiring_soon", Title: "b", DedupeKey: "conn:acc:soon"}
	ok, err = repo.Insert(ctx, dup)
	if err != nil {
		t.Fatalf("dup insert err: %v", err)
	}
	if ok {
		t.Fatalf("dup should have collapsed, but inserted")
	}

	// A different user with the same key is NOT a collision.
	other := &models.Notification{ID: "d3", UserID: "u2", Level: models.NotificationLevelWarning, Type: "connection.expiring_soon", Title: "c", DedupeKey: "conn:acc:soon"}
	if ok, err := repo.Insert(ctx, other); err != nil || !ok {
		t.Fatalf("other-user insert: ok=%v err=%v", ok, err)
	}

	// Once the first is read, the same key inserts fresh (partial index only
	// covers unread rows).
	if _, err := repo.SetRead(ctx, "u1", "d1", true); err != nil {
		t.Fatalf("set read: %v", err)
	}
	reafter := &models.Notification{ID: "d4", UserID: "u1", Level: models.NotificationLevelWarning, Type: "connection.expiring_soon", Title: "d", DedupeKey: "conn:acc:soon"}
	if ok, err := repo.Insert(ctx, reafter); err != nil || !ok {
		t.Fatalf("post-read insert: ok=%v err=%v", ok, err)
	}
}

func TestNotifications_ListReadCountDismiss(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	for _, id := range []string{"a", "b", "c"} {
		if _, err := repo.Insert(ctx, newNotif(id, "u1", "x")); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Newest-first.
	list, err := repo.List(ctx, "u1", repository.NotificationListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 || list[0].ID != "c" || list[2].ID != "a" {
		t.Fatalf("unexpected order/len: %+v", ids(list))
	}

	if n, _ := repo.UnreadCount(ctx, "u1"); n != 3 {
		t.Fatalf("unread count = %d, want 3", n)
	}

	// Mark one read → unread filter drops it, count falls.
	if ok, err := repo.SetRead(ctx, "u1", "b", true); err != nil || !ok {
		t.Fatalf("set read b: ok=%v err=%v", ok, err)
	}
	unread, _ := repo.List(ctx, "u1", repository.NotificationListOpts{UnreadOnly: true})
	if len(unread) != 2 {
		t.Fatalf("unread list len = %d, want 2", len(unread))
	}
	if n, _ := repo.UnreadCount(ctx, "u1"); n != 2 {
		t.Fatalf("unread count = %d, want 2", n)
	}

	// Dismiss → gone from list and count.
	if ok, err := repo.Dismiss(ctx, "u1", "c"); err != nil || !ok {
		t.Fatalf("dismiss c: ok=%v err=%v", ok, err)
	}
	list, _ = repo.List(ctx, "u1", repository.NotificationListOpts{})
	if len(list) != 2 {
		t.Fatalf("after dismiss len = %d, want 2", len(list))
	}

	// A second dismiss of the same row matches nothing.
	if ok, _ := repo.Dismiss(ctx, "u1", "c"); ok {
		t.Fatalf("re-dismiss should not match")
	}
	// An unknown/foreign id matches nothing (→ handler 404).
	if ok, _ := repo.SetRead(ctx, "u1", "nope", true); ok {
		t.Fatalf("set read of unknown id should not match")
	}
}

func TestNotifications_MarkAllReadRespectsBefore(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	seq := make(map[string]int64)
	for _, id := range []string{"a", "b", "c"} {
		n := newNotif(id, "u1", "x")
		if _, err := repo.Insert(ctx, n); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		seq[id] = n.Seq
	}

	// Mark all read up to b's seq → a and b read, c stays unread.
	updated, err := repo.MarkAllRead(ctx, "u1", seq["b"])
	if err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2", updated)
	}
	if n, _ := repo.UnreadCount(ctx, "u1"); n != 1 {
		t.Fatalf("unread after bounded mark-all = %d, want 1", n)
	}

	// Unbounded marks the rest.
	if updated, _ := repo.MarkAllRead(ctx, "u1", 0); updated != 1 {
		t.Fatalf("unbounded mark-all updated = %d, want 1", updated)
	}
	if n, _ := repo.UnreadCount(ctx, "u1"); n != 0 {
		t.Fatalf("unread after full mark-all = %d, want 0", n)
	}
}

func TestNotifications_ReplaySinceAscending(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	var seqs []int64
	for _, id := range []string{"a", "b", "c", "d"} {
		n := newNotif(id, "u1", "x")
		if _, err := repo.Insert(ctx, n); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		seqs = append(seqs, n.Seq)
	}
	// Replay after the 2nd row's seq → c, d ascending.
	got, err := repo.ReplaySince(ctx, "u1", seqs[1], 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "d" {
		t.Fatalf("replay = %+v, want [c d]", ids(got))
	}
}

// TestNotifications_ReadsHydrateSeq guards the CON-242 bug where a `scanonly`
// tag on Seq dropped the column from the generated SELECT, so every List /
// ReplaySince row came back seq=0 (breaking replay cursors and mark-all-read's
// `before` bound) while Insert still populated Seq via its explicit RETURNING —
// which is why the other tests, all reading Seq off the *inserted* model,
// stayed green. Here we assert Seq on rows coming back from a read.
func TestNotifications_ReadsHydrateSeq(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	inserted := make(map[string]int64)
	for _, id := range []string{"a", "b", "c"} {
		n := newNotif(id, "u1", "x")
		if _, err := repo.Insert(ctx, n); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if n.Seq == 0 {
			t.Fatalf("insert %s did not populate Seq", id)
		}
		inserted[id] = n.Seq
	}

	list, err := repo.List(ctx, "u1", repository.NotificationListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Guard against a vacuous pass: the per-row seq checks below only exercise
	// hydration if List actually returned the seeded rows. An empty (or short)
	// list would slip through the loop silently.
	if len(list) != 3 {
		t.Fatalf("List returned %d rows, want the 3 seeded (a, b, c)", len(list))
	}
	for _, n := range list {
		if n.Seq == 0 {
			t.Fatalf("List returned seq=0 for %s (column dropped from SELECT)", n.ID)
		}
		if n.Seq != inserted[n.ID] {
			t.Fatalf("List seq for %s = %d, want %d", n.ID, n.Seq, inserted[n.ID])
		}
	}

	// A cursor built from a List row must actually advance ReplaySince — the
	// downstream effect the bug nullified. Page after the oldest row's seq.
	replay, err := repo.ReplaySince(ctx, "u1", inserted["a"], 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replay) != 2 || replay[0].ID != "b" || replay[1].ID != "c" {
		t.Fatalf("replay after a = %+v, want [b c]", ids(replay))
	}
	for _, n := range replay {
		if n.Seq != inserted[n.ID] {
			t.Fatalf("ReplaySince seq for %s = %d, want %d", n.ID, n.Seq, inserted[n.ID])
		}
	}
}

func TestNotifications_TenantIsolation(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)

	if _, err := repo.Insert(tctx("t1"), newNotif("x1", "u1", "x")); err != nil {
		t.Fatalf("insert t1: %v", err)
	}
	// Reading the SAME user under a different tenant must see nothing — the
	// TenantScoped hook filters by the active tenant.
	list, err := repo.List(tctx("t2"), "u1", repository.NotificationListOpts{})
	if err != nil {
		t.Fatalf("list t2: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("cross-tenant read leaked %d rows", len(list))
	}
}

func TestNotifications_DeleteExpiredAndRetention(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	past := time.Now().UTC().Add(-time.Hour)
	expired := newNotif("exp", "u1", "x")
	expired.ExpiresAt = &past
	if _, err := repo.Insert(ctx, expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	fresh := newNotif("fresh", "u1", "x")
	if _, err := repo.Insert(ctx, fresh); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}

	// retention=0 reaps ONLY expired rows. Runs cross-tenant → system context.
	sys := tenantctx.WithSystem(t.Context())
	n, err := repo.DeleteExpired(sys, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1 (only the expired row)", n)
	}
	remaining, _ := repo.List(ctx, "u1", repository.NotificationListOpts{})
	if len(remaining) != 1 || remaining[0].ID != "fresh" {
		t.Fatalf("after expiry sweep: %+v, want [fresh]", ids(remaining))
	}

	// Age a read row past the retention window (direct SQL — SetRead stamps now)
	// and confirm the retention branch reaps it.
	old := newNotif("oldread", "u1", "x")
	if _, err := repo.Insert(ctx, old); err != nil {
		t.Fatalf("insert oldread: %v", err)
	}
	if _, err := db.NewUpdate().Model((*models.Notification)(nil)).
		Set("read_at = ?", time.Now().UTC().Add(-48*time.Hour)).
		Where("id = ?", "oldread").Exec(sys); err != nil {
		t.Fatalf("age read_at: %v", err)
	}
	n, err = repo.DeleteExpired(sys, time.Now().UTC(), 24*time.Hour)
	if err != nil {
		t.Fatalf("delete retention: %v", err)
	}
	if n != 1 {
		t.Fatalf("retention deleted %d, want 1", n)
	}
}

func TestNotifications_ExpiredExcludedFromReads(t *testing.T) {
	db := openNotifDB(t)
	repo := repository.NewNotificationRepository(db)
	ctx := tctx("t1")

	fresh := newNotif("fresh", "u1", "x")
	if _, err := repo.Insert(ctx, fresh); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}
	// Expired at insert time — NOT cleaned up yet. It must still be excluded from
	// every live-inbox read (read-time fading), not just after the cleanup sweep.
	past := time.Now().UTC().Add(-time.Hour)
	expired := newNotif("expired", "u1", "x")
	expired.ExpiresAt = &past
	if _, err := repo.Insert(ctx, expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}

	list, err := repo.List(ctx, "u1", repository.NotificationListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "fresh" {
		t.Fatalf("list = %+v, want [fresh] (expired excluded)", ids(list))
	}

	if n, _ := repo.UnreadCount(ctx, "u1"); n != 1 {
		t.Fatalf("unread count = %d, want 1 (expired excluded)", n)
	}

	replay, err := repo.ReplaySince(ctx, "u1", 0, 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replay) != 1 || replay[0].ID != "fresh" {
		t.Fatalf("replay = %+v, want [fresh] (expired excluded)", ids(replay))
	}
}

func ids(list []models.Notification) []string {
	out := make([]string, len(list))
	for i := range list {
		out[i] = list[i].ID
	}
	return out
}
