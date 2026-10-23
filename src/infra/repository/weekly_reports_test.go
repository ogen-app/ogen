package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func weeklyWindow() repository.ReportWindow {
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	return repository.ReportWindow{
		PrevStart: start.AddDate(0, 0, -7),
		Start:     start,
		End:       start.AddDate(0, 0, 7),
		Now:       start.AddDate(0, 0, 7).Add(9 * time.Hour),
	}
}

// TestWeeklyReportFacts runs every report query against the real schema and
// checks the publishing tallies: posts by publish time, failures counted once
// per post at their latest transition, and the week before kept apart.
func TestWeeklyReportFacts(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewWeeklyReportRepository(db)
	ctx := tenantCtx()
	tid := models.DefaultTenantID
	w := weeklyWindow()

	seedPost(t, db, "p-in-1", "zernio", "z1", w.Start.Add(2*time.Hour))
	seedPost(t, db, "p-in-2", "zernio", "z2", w.Start.Add(50*time.Hour))
	seedPost(t, db, "p-prev", "zernio", "z3", w.PrevStart.Add(time.Hour))
	seedPost(t, db, "p-manual", "manual", "", w.Start.Add(3*time.Hour))
	seedPost(t, db, "p-failed", "zernio", "", w.PrevStart.AddDate(0, 0, -1))

	failed, notPub := models.PostStatusFailed, models.PostStatusNotPublished
	sched := models.PostStatusScheduled
	seedPostLog(t, db, "l1", "p-failed", models.PostLogEventStateTransition, "system", &sched, &failed, w.Start.Add(10*time.Hour), "")
	seedPostLog(t, db, "l2", "p-failed", models.PostLogEventStateTransition, "system", &sched, &notPub, w.Start.Add(20*time.Hour), "")
	seedPostLog(t, db, "l3", "p-prev", models.PostLogEventStateTransition, "system", &sched, &failed, w.PrevStart.Add(2*time.Hour), "")

	c, err := repo.Counts(ctx, tid, w)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.PrevPublished != 1 || c.Failed != 1 || c.PrevFailed != 1 {
		t.Fatalf("counts = %+v", c)
	}

	pub, err := repo.PublishedPosts(ctx, tid, w.Start, w.End)
	if err != nil {
		t.Fatalf("published: %v", err)
	}
	if len(pub) != 2 || pub[0].ID != "p-in-1" || pub[1].ID != "p-in-2" || pub[0].CampaignID != "camp-1" {
		t.Fatalf("published = %+v", pub)
	}

	fails, err := repo.Failures(ctx, tid, w.Start, w.End, 10)
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	if len(fails) != 1 || fails[0].PostID != "p-failed" || !fails[0].At.Equal(w.Start.Add(20*time.Hour)) {
		t.Fatalf("failures = %+v", fails)
	}

	if _, err := repo.AttentionAccounts(ctx, tid, w.Now); err != nil {
		t.Fatalf("attention: %v", err)
	}
	if _, err := repo.GoalCampaigns(ctx, tid); err != nil {
		t.Fatalf("goal campaigns: %v", err)
	}
	if other, err := repo.Counts(ctx, "another-tenant", w); err != nil || other.Failed != 0 || other.PrevPublished != 0 {
		t.Fatalf("other tenant counts = %+v, %v", other, err)
	}
}

// TestWeeklyReportTenantsAndOptOut covers the active-tenant reads and the
// per-membership opt-out that picks the recipients.
func TestWeeklyReportTenantsAndOptOut(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewWeeklyReportRepository(db)
	ctx := tenantCtx()
	tid := models.DefaultTenantID

	seedTenant(t, db, "tn-active", "Active", "default")
	seedTenant(t, db, "tn-frozen", "Frozen", "default")
	if _, err := db.NewUpdate().Table("tenants").Set("status = 'suspended'").Where("id = 'tn-frozen'").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	tenants, err := repo.ListActiveTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, tn := range tenants {
		ids[tn.ID] = true
	}
	if !ids["tn-active"] || ids["tn-frozen"] {
		t.Fatalf("active tenants = %+v", tenants)
	}
	if name, err := repo.TenantName(ctx, "tn-active"); err != nil || name != "Active" {
		t.Fatalf("name = %q, %v", name, err)
	}
	if _, err := repo.TenantName(ctx, "tn-frozen"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("suspended tenant name: %v", err)
	}

	seedUser(t, db, "owner-1", tid, "o1@x.com")
	seedUser(t, db, "owner-2", tid, "o2@x.com")
	seedUser(t, db, "member-1", tid, "m1@x.com")
	if _, err := db.NewUpdate().Table("users").Set("role = 'owner'").Where("id IN ('owner-1', 'owner-2')").Exec(ctx); err != nil {
		t.Fatalf("promote owners: %v", err)
	}
	if err := repo.SetOptOut(ctx, "owner-2", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	if out, err := repo.OptedOut(ctx, "owner-2"); err != nil || !out {
		t.Fatalf("opted out = %v, %v", out, err)
	}
	rcpts, err := repo.ListRecipients(ctx, tid)
	if err != nil || len(rcpts) != 1 || rcpts[0].ID != "owner-1" {
		t.Fatalf("recipients = %+v, %v", rcpts, err)
	}
	if err := repo.SetOptOut(ctx, "nobody", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing membership: %v", err)
	}
}

// TestActivityCountDistinctUsers counts each member once, ignores system
// events and other tenants, and honours the window bounds.
func TestActivityCountDistinctUsers(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewActivityRepository(db)
	ctx := tenantCtx()
	w := weeklyWindow()
	ev := func(id, user string, at time.Time) *models.ActivityEvent {
		return &models.ActivityEvent{ID: id, UserID: user, Category: "post", Type: "post_created", OccurredAt: at}
	}
	if err := repo.Insert(ctx, []*models.ActivityEvent{
		ev("a1", "u1", w.Start.Add(time.Hour)),
		ev("a2", "u1", w.Start.Add(2*time.Hour)),
		ev("a3", "u2", w.End.Add(-time.Minute)),
		ev("a4", "", w.Start.Add(time.Hour)),
		ev("a5", "u3", w.End),
		ev("a6", "u4", w.Start.Add(-time.Minute)),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := repo.CountDistinctUsers(ctx, models.DefaultTenantID, w.Start, w.End)
	if err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2", n, err)
	}
	if n, _ := repo.CountDistinctUsers(ctx, "another-tenant", w.Start, w.End); n != 0 {
		t.Fatalf("other tenant count = %d", n)
	}
}
