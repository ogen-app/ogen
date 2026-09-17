package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// announcement tests use openMigratedDB (FK enforcement off) so tenants/users/
// group-assignments can be seeded with synthetic ids without full FK chains. The
// behaviour under test — targeting, per-user tracking, stats, lifecycle — does
// not depend on FK actions (the CASCADE guarantees are schema-level).

func seedGroupAssignment(t *testing.T, db *bun.DB, tenantID, groupID string) {
	t.Helper()
	row := &models.TenantGroupAssignment{TenantID: tenantID, GroupID: groupID, CreatedAt: time.Now().UTC()}
	if _, err := db.NewInsert().Model(row).Exec(t.Context()); err != nil {
		t.Fatalf("seed group assignment: %v", err)
	}
}

// publishAnnouncement creates a draft and moves it to published (stamping
// published_at), returning its id.
func publishAnnouncement(t *testing.T, repo repository.AnnouncementRepository, a *models.Announcement, groupIDs, tierIDs []string) string {
	t.Helper()
	ctx := context.Background()
	if err := repo.Create(ctx, a, groupIDs, tierIDs); err != nil {
		t.Fatalf("create announcement: %v", err)
	}
	if ok, err := repo.SetStatus(ctx, a.ID, models.AnnouncementStatusPublished); err != nil || !ok {
		t.Fatalf("publish announcement: ok=%v err=%v", ok, err)
	}
	return a.ID
}

func idsOf(rows []repository.AnnouncementForUser) map[string]repository.AnnouncementForUser {
	out := make(map[string]repository.AnnouncementForUser, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// TestAnnouncementDeliveryTargeting covers ActiveForTenant: all / tier / group
// targeting, and exclusion of drafts, expired, future and dismissed banners.
func TestAnnouncementDeliveryTargeting(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAnnouncementRepository(db)
	ctx := context.Background()

	seedTenant(t, db, "tn-all", "All", "default")
	seedTenant(t, db, "tn-pro", "Pro", "pro")
	seedTenant(t, db, "tn-grp", "Grp", "default")
	seedUser(t, db, "u-all", "tn-all", "all@x.com")
	seedUser(t, db, "u-pro", "tn-pro", "pro@x.com")
	seedUser(t, db, "u-grp", "tn-grp", "grp@x.com")
	seedGroupAssignment(t, db, "tn-grp", "g1")

	annAll := publishAnnouncement(t, repo, &models.Announcement{Title: "All", Body: "everyone", TargetAll: true}, nil, nil)
	annPro := publishAnnouncement(t, repo, &models.Announcement{Title: "Pro", Body: "pro tier"}, nil, []string{"pro"})
	annGrp := publishAnnouncement(t, repo, &models.Announcement{Title: "Grp", Body: "group g1"}, []string{"g1"}, nil)
	// A draft never delivers.
	if err := repo.Create(ctx, &models.Announcement{Title: "Draft", Body: "wip", TargetAll: true}, nil, nil); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	// Expired (ends in the past) + future (starts later) never deliver even though
	// published + target_all.
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	publishAnnouncement(t, repo, &models.Announcement{Title: "Expired", Body: "gone", TargetAll: true, EndsAt: &past}, nil, nil)
	publishAnnouncement(t, repo, &models.Announcement{Title: "Future", Body: "soon", TargetAll: true, StartsAt: &future}, nil, nil)

	// u-all (default tier, no groups) → only the all-target banner.
	got, err := repo.ActiveForTenant(ctx, repository.AnnouncementAudience{TenantID: "tn-all", TierID: "default", UserID: "u-all"})
	if err != nil {
		t.Fatalf("active for tn-all: %v", err)
	}
	if m := idsOf(got); len(m) != 1 || func() bool { _, ok := m[annAll]; return !ok }() {
		t.Fatalf("tn-all: want only %s, got %v", annAll, keys(got))
	}

	// u-pro (pro tier) → all-target + pro-tier banner.
	got, err = repo.ActiveForTenant(ctx, repository.AnnouncementAudience{TenantID: "tn-pro", TierID: "pro", UserID: "u-pro"})
	if err != nil {
		t.Fatalf("active for tn-pro: %v", err)
	}
	if m := idsOf(got); len(m) != 2 || !has(m, annAll) || !has(m, annPro) {
		t.Fatalf("tn-pro: want %s+%s, got %v", annAll, annPro, keys(got))
	}

	// u-grp (default tier, group g1) → all-target + group banner.
	got, err = repo.ActiveForTenant(ctx, repository.AnnouncementAudience{TenantID: "tn-grp", TierID: "default", GroupIDs: []string{"g1"}, UserID: "u-grp"})
	if err != nil {
		t.Fatalf("active for tn-grp: %v", err)
	}
	if m := idsOf(got); len(m) != 2 || !has(m, annAll) || !has(m, annGrp) {
		t.Fatalf("tn-grp: want %s+%s, got %v", annAll, annGrp, keys(got))
	}

	// Dismissing hides it for that user only.
	if ok, err := repo.RecordDismiss(ctx, annAll, repository.AnnouncementAudience{TenantID: "tn-all", TierID: "default", UserID: "u-all"}); err != nil || !ok {
		t.Fatalf("dismiss: ok=%v err=%v", ok, err)
	}
	got, err = repo.ActiveForTenant(ctx, repository.AnnouncementAudience{TenantID: "tn-all", TierID: "default", UserID: "u-all"})
	if err != nil {
		t.Fatalf("active after dismiss: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tn-all after dismiss: want empty, got %v", keys(got))
	}
	// A different user in the same-target set is unaffected.
	got, err = repo.ActiveForTenant(ctx, repository.AnnouncementAudience{TenantID: "tn-pro", TierID: "pro", UserID: "u-pro"})
	if err != nil {
		t.Fatalf("active u-pro after u-all dismiss: %v", err)
	}
	if !has(idsOf(got), annAll) {
		t.Fatalf("u-pro should still see %s", annAll)
	}
}

// TestAnnouncementClickIdempotent covers RecordClick: first-click-wins,
// idempotency, the Clicked flag on delivery, and 404 for unknown/unpublished.
func TestAnnouncementClickIdempotent(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAnnouncementRepository(db)
	ctx := context.Background()

	seedTenant(t, db, "tn-a", "A", "default")
	seedUser(t, db, "u-a", "tn-a", "a@x.com")
	ann := publishAnnouncement(t, repo, &models.Announcement{Title: "Hi", Body: "b", TargetAll: true, CTALabel: "Go", CTAURL: "https://x/y"}, nil, nil)
	audA := repository.AnnouncementAudience{TenantID: "tn-a", TierID: "default", UserID: "u-a"}

	if ok, err := repo.RecordClick(ctx, ann, audA); err != nil || !ok {
		t.Fatalf("click 1: ok=%v err=%v", ok, err)
	}
	first := getInteraction(t, db, ann, "u-a")
	if first.ClickedAt == nil {
		t.Fatal("clicked_at should be set")
	}
	// Second click keeps the first timestamp (first-click-wins).
	time.Sleep(2 * time.Millisecond)
	if ok, err := repo.RecordClick(ctx, ann, audA); err != nil || !ok {
		t.Fatalf("click 2: ok=%v err=%v", ok, err)
	}
	second := getInteraction(t, db, ann, "u-a")
	if !second.ClickedAt.Equal(*first.ClickedAt) {
		t.Fatalf("clicked_at moved: %v -> %v", first.ClickedAt, second.ClickedAt)
	}

	// Delivery reports Clicked=true and the click did NOT hide it.
	got, err := repo.ActiveForTenant(ctx, repository.AnnouncementAudience{TenantID: "tn-a", TierID: "default", UserID: "u-a"})
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	m := idsOf(got)
	if !has(m, ann) || !m[ann].Clicked {
		t.Fatalf("want %s present with Clicked=true, got %v", ann, got)
	}

	// Unknown id and an unpublished (draft) id both report not-found.
	if ok, _ := repo.RecordClick(ctx, "nope", audA); ok {
		t.Fatal("click unknown should be false")
	}
	draft := &models.Announcement{Title: "D", Body: "b", TargetAll: true}
	if err := repo.Create(ctx, draft, nil, nil); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if ok, _ := repo.RecordClick(ctx, draft.ID, audA); ok {
		t.Fatal("click on draft should be false")
	}

	// A tenant NOT in the announcement's audience can't record an interaction
	// (targeting gate — no stats-inflation via a crafted id).
	proOnly := publishAnnouncement(t, repo, &models.Announcement{Title: "Pro", Body: "b"}, nil, []string{"pro"})
	if ok, _ := repo.RecordClick(ctx, proOnly, audA); ok { // audA is the default tier, not pro
		t.Fatal("click by a non-targeted tenant should be false")
	}
	if ok, err := repo.RecordClick(ctx, proOnly, repository.AnnouncementAudience{TenantID: "tn-p", TierID: "pro", UserID: "u-p"}); err != nil || !ok {
		t.Fatalf("click by a targeted tenant should succeed: ok=%v err=%v", ok, err)
	}
}

// TestAnnouncementStats covers the engagement rollup + eligible-audience
// denominator, including per-tenant distinct rollup.
func TestAnnouncementStats(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAnnouncementRepository(db)
	ctx := context.Background()

	// Two tenants on the pro tier; one has two users.
	seedTenant(t, db, "tn-1", "One", "pro")
	seedTenant(t, db, "tn-2", "Two", "pro")
	seedUser(t, db, "u-1a", "tn-1", "1a@x.com")
	seedUser(t, db, "u-1b", "tn-1", "1b@x.com")
	seedUser(t, db, "u-2", "tn-2", "2@x.com")

	ann := publishAnnouncement(t, repo, &models.Announcement{Title: "Pro", Body: "b"}, nil, []string{"pro"})

	// Two users in tn-1 click, plus u-2; u-2 also dismisses. All on the pro tier
	// the announcement targets.
	mustClick(t, repo, ann, repository.AnnouncementAudience{TenantID: "tn-1", TierID: "pro", UserID: "u-1a"})
	mustClick(t, repo, ann, repository.AnnouncementAudience{TenantID: "tn-1", TierID: "pro", UserID: "u-1b"})
	mustClick(t, repo, ann, repository.AnnouncementAudience{TenantID: "tn-2", TierID: "pro", UserID: "u-2"})
	if ok, err := repo.RecordDismiss(ctx, ann, repository.AnnouncementAudience{TenantID: "tn-2", TierID: "pro", UserID: "u-2"}); err != nil || !ok {
		t.Fatalf("dismiss: ok=%v err=%v", ok, err)
	}

	st, err := repo.Stats(ctx, ann)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.UniqueUsersClicked != 3 {
		t.Errorf("users clicked: got %d want 3", st.UniqueUsersClicked)
	}
	if st.UniqueTenantsClicked != 2 {
		t.Errorf("tenants clicked: got %d want 2", st.UniqueTenantsClicked)
	}
	if st.UniqueUsersDismissed != 1 || st.UniqueTenantsDismissed != 1 {
		t.Errorf("dismissed: users=%d tenants=%d want 1/1", st.UniqueUsersDismissed, st.UniqueTenantsDismissed)
	}
	// Eligible = active tenants on the pro tier (tn-1, tn-2) and their 3 users.
	if st.EligibleTenants != 2 {
		t.Errorf("eligible tenants: got %d want 2", st.EligibleTenants)
	}
	if st.EligibleUsers != 3 {
		t.Errorf("eligible users: got %d want 3", st.EligibleUsers)
	}

	// Stats for an unknown id is ErrNoRows.
	if _, err := repo.Stats(ctx, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stats unknown: got %v want ErrNoRows", err)
	}
}

// TestAnnouncementListAndLifecycle covers Get hydration, List (status filter +
// keyset paging), SetStatus publish stamping, and draft-only Delete.
func TestAnnouncementListAndLifecycle(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAnnouncementRepository(db)
	ctx := context.Background()

	// Create draft with targeting, then Get hydrates the target ids.
	a := &models.Announcement{Title: "T", Body: "b"}
	if err := repo.Create(ctx, a, []string{"g1", "g1", ""}, []string{"pro"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.TargetGroupIDs) != 1 || got.TargetGroupIDs[0] != "g1" {
		t.Fatalf("target groups: got %v want [g1] (deduped, blanks dropped)", got.TargetGroupIDs)
	}
	if len(got.TargetTierIDs) != 1 || got.TargetTierIDs[0] != "pro" {
		t.Fatalf("target tiers: got %v want [pro]", got.TargetTierIDs)
	}
	if got.PublishedAt != nil {
		t.Fatal("draft should have no published_at")
	}

	// Publish stamps published_at.
	if ok, err := repo.SetStatus(ctx, a.ID, models.AnnouncementStatusPublished); err != nil || !ok {
		t.Fatalf("publish: ok=%v err=%v", ok, err)
	}
	got, _ = repo.Get(ctx, a.ID)
	if got.PublishedAt == nil {
		t.Fatal("published should stamp published_at")
	}
	stamp := *got.PublishedAt
	// Re-publish keeps the original stamp.
	if _, err := repo.SetStatus(ctx, a.ID, models.AnnouncementStatusPublished); err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	got, _ = repo.Get(ctx, a.ID)
	if !got.PublishedAt.Equal(stamp) {
		t.Fatalf("published_at moved on re-publish: %v -> %v", stamp, got.PublishedAt)
	}

	// Update replaces targeting.
	got.Title = "T2"
	if ok, err := repo.Update(ctx, got, nil, nil); err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	reloaded, _ := repo.Get(ctx, a.ID)
	if reloaded.Title != "T2" || len(reloaded.TargetTierIDs) != 0 || len(reloaded.TargetGroupIDs) != 0 {
		t.Fatalf("update: title=%q tiers=%v groups=%v", reloaded.Title, reloaded.TargetTierIDs, reloaded.TargetGroupIDs)
	}

	// A published announcement can't be deleted (found, not deleted); archive it,
	// still retained; only a draft deletes.
	if found, deleted, err := repo.Delete(ctx, a.ID); err != nil || !found || deleted {
		t.Fatalf("delete published: found=%v deleted=%v err=%v", found, deleted, err)
	}
	if found, deleted, _ := repo.Delete(ctx, "nope"); found || deleted {
		t.Fatalf("delete unknown: found=%v deleted=%v", found, deleted)
	}
	draft := &models.Announcement{Title: "D", Body: "b"}
	if err := repo.Create(ctx, draft, nil, nil); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if found, deleted, err := repo.Delete(ctx, draft.ID); err != nil || !found || !deleted {
		t.Fatalf("delete draft: found=%v deleted=%v err=%v", found, deleted, err)
	}
	if _, err := repo.Get(ctx, draft.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted draft get: got %v want ErrNoRows", err)
	}

	// List: status filter + keyset paging over created_at DESC, id DESC.
	all, err := repo.List(ctx, repository.AnnouncementListFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) < 1 {
		t.Fatalf("list all: want >=1, got %d", len(all))
	}
	pub, err := repo.List(ctx, repository.AnnouncementListFilter{Status: models.AnnouncementStatusPublished})
	if err != nil {
		t.Fatalf("list published: %v", err)
	}
	for _, an := range pub {
		if an.Status != models.AnnouncementStatusPublished {
			t.Fatalf("status filter leaked %q", an.Status)
		}
	}
	// Page size 1 returns one row and a stable cursor advances.
	page1, err := repo.List(ctx, repository.AnnouncementListFilter{Limit: 1})
	if err != nil || len(page1) != 1 {
		t.Fatalf("page1: len=%d err=%v", len(page1), err)
	}
	page2, err := repo.List(ctx, repository.AnnouncementListFilter{
		Limit:           1,
		CursorCreatedAt: page1[0].CreatedAt,
		CursorID:        page1[0].ID,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) == 1 && page2[0].ID == page1[0].ID {
		t.Fatal("keyset cursor did not advance")
	}
}

func mustClick(t *testing.T, repo repository.AnnouncementRepository, annID string, aud repository.AnnouncementAudience) {
	t.Helper()
	if ok, err := repo.RecordClick(context.Background(), annID, aud); err != nil || !ok {
		t.Fatalf("click %s/%s: ok=%v err=%v", annID, aud.UserID, ok, err)
	}
}

func getInteraction(t *testing.T, db *bun.DB, annID, userID string) models.AnnouncementInteraction {
	t.Helper()
	var row models.AnnouncementInteraction
	if err := db.NewSelect().Model(&row).
		Where("announcement_id = ?", annID).Where("user_id = ?", userID).
		Scan(t.Context()); err != nil {
		t.Fatalf("get interaction: %v", err)
	}
	return row
}

func has(m map[string]repository.AnnouncementForUser, id string) bool {
	_, ok := m[id]
	return ok
}

func keys(rows []repository.AnnouncementForUser) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
