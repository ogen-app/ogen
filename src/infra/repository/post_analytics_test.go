package repository_test

import (
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// openMigratedDB returns a fresh, fully-migrated Postgres database with FK
// enforcement bypassed for the session, so a test can insert a post with a
// synthetic campaign_id / created_by without seeding the whole campaign
// graph — the repo logic under test doesn't depend on FK enforcement.
//
// It also applies the analytics migration set on the SAME database, so the
// post_analytics_snapshots table (CON-125 Track B) exists here. In production
// that table lives in the isolated analytics DB; in tests one physical DB holds
// both sets (the table names don't collide), so the repo can be pointed at the
// same pool.
//
// Postgres has no per-statement FK pragma, so the pool is capped at 1 and
// the session uses session_replication_role=replica (which skips FK-trigger
// checks; the test database user is a superuser, as required). With a single
// pooled connection the setting sticks for the whole test.
func openMigratedDB(t *testing.T) *bun.DB {
	t.Helper()
	db := pgtest.MustDB()
	// Apply the analytics migrations FIRST, in normal replication mode. The
	// CON-236 migration rebuilds the post_analytics_snapshots hypertable
	// (drop + rename), and TimescaleDB maintains its internal catalog via
	// triggers that session_replication_role=replica would skip — which corrupts
	// the drop (a stale hypertable catalog row survives). So migrate before
	// flipping to replica for the FK-free seeding below.
	if err := database.MigrateAnalytics(t.Context(), db); err != nil {
		t.Fatalf("migrate analytics: %v", err)
	}
	db.DB.SetMaxOpenConns(1)
	db.DB.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedPost(t *testing.T, db *bun.DB, id, publisher, publisherPostID string, publishedAt time.Time) {
	t.Helper()
	post := &models.Post{
		ID:              id,
		CampaignID:      "camp-1",
		Title:           "Post " + id,
		Content:         "content " + id,
		MediaURLs:       models.StringSlice{},
		UsedAssetIDs:    models.StringSlice{},
		Status:          models.PostStatusPublished,
		Publisher:       publisher,
		PublisherPostID: publisherPostID,
		CTAType:         models.CTATypeNone,
		CreatedBy:       "user-1",
		PublishedAt:     &publishedAt,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if _, err := db.NewInsert().Model(post).Exec(tenantCtx()); err != nil {
		t.Fatalf("seed post %s: %v", id, err)
	}
}

func TestPostAnalyticsUpsertInPlaceAndGet(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostAnalyticsRepository(db)

	// No current row yet → (nil, nil).
	got, err := repo.GetByPostID(ctx, "p1")
	if err != nil {
		t.Fatalf("get (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}

	lu := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	t0 := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	first := &models.PostAnalytics{
		PostID:          "p1",
		PublisherPostID: "z-1",
		Publisher:       models.PublisherZernio,
		Platform:        "linkedin",
		Impressions:     100,
		Likes:           10,
		EngagementRate:  0.1,
		PlatformAnalytics: models.PlatformAnalyticsList{
			{Platform: "linkedin", SyncStatus: "synced", Analytics: models.PostAnalyticsMetrics{Impressions: 100, Likes: 10}},
		},
		SyncStatus:         "synced",
		MetricsLastUpdated: &lu,
		FirstSeenAt:        t0,
		LastChangedAt:      t0,
		LastCheckedAt:      t0,
	}
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err = repo.GetByPostID(ctx, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Impressions != 100 || got.Likes != 10 {
		t.Fatalf("current mismatch: %+v", got)
	}
	if len(got.PlatformAnalytics) != 1 || got.PlatformAnalytics[0].Platform != "linkedin" {
		t.Fatalf("platform breakdown not persisted: %+v", got.PlatformAnalytics)
	}

	// A later refresh UPSERTS in place — same (tenant, post) row, no second row.
	second := *first
	second.Impressions = 250
	second.LastChangedAt = t0.Add(30 * time.Minute)
	second.LastCheckedAt = t0.Add(30 * time.Minute)
	if err := repo.Upsert(ctx, &second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	got, _ = repo.GetByPostID(ctx, "p1")
	if got.Impressions != 250 {
		t.Fatalf("upsert-in-place failed: impressions=%d want 250", got.Impressions)
	}
	count, err := db.NewSelect().Model((*models.PostAnalytics)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 current row after upsert-in-place, got %d", count)
	}

	// Trend history is a separate append-only table.
	snap := got.NewSnapshot(mustID(t), t0.Add(30*time.Minute))
	if err := repo.AppendSnapshot(ctx, snap); err != nil {
		t.Fatalf("append snapshot: %v", err)
	}
	hist, err := db.NewSelect().Model((*models.PostAnalyticsSnapshot)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("history count: %v", err)
	}
	if hist != 1 {
		t.Fatalf("expected 1 history row, got %d", hist)
	}
}

func TestPostAnalyticsListAndOverview(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostAnalyticsRepository(db)

	base := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	mustUpsert(t, repo, "p1", "z-1", 100, 10, 0.10, base)
	mustUpsert(t, repo, "p2", "z-2", 300, 30, 0.20, base)
	// A later check for p1 upserts in place (250) — the overview reads current.
	mustUpsert(t, repo, "p1", "z-1", 250, 10, 0.10, base.Add(30*time.Minute))

	items, overview, err := repo.List(ctx, repository.PostAnalyticsListOptions{
		Publisher: models.PublisherZernio,
		SortBy:    "impressions",
		Order:     "desc",
		Page:      1,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items: got %d want 2", len(items))
	}
	// Sorted by impressions desc → p2 (300) first, then p1 (its current 250).
	if items[0].PostID != "p2" || items[1].PostID != "p1" {
		t.Fatalf("sort order wrong: %s, %s", items[0].PostID, items[1].PostID)
	}
	if items[0].Analytics.Impressions != 300 || items[1].Analytics.Impressions != 250 {
		t.Fatalf("current-state nested analytics wrong: %+v / %+v", items[0].Analytics, items[1].Analytics)
	}
	// last_refreshed_at is sourced from last_checked_at.
	if !items[1].LastRefreshedAt.Equal(base.Add(30 * time.Minute)) {
		t.Fatalf("last_refreshed_at should track last_checked_at: %v", items[1].LastRefreshedAt)
	}
	if overview.PostCount != 2 || overview.Impressions != 550 || overview.Likes != 40 {
		t.Fatalf("overview sums wrong: %+v", overview)
	}
	if overview.EngagementRateAvg < 0.149 || overview.EngagementRateAvg > 0.151 {
		t.Fatalf("overview avg wrong: %v", overview.EngagementRateAvg)
	}

	// Pagination: limit 1 returns the top item, overview still over full set.
	page1, ov, err := repo.List(ctx, repository.PostAnalyticsListOptions{
		Publisher: models.PublisherZernio, SortBy: "impressions", Order: "desc", Page: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if len(page1) != 1 || page1[0].PostID != "p2" {
		t.Fatalf("page1 wrong: %+v", page1)
	}
	if ov.PostCount != 2 {
		t.Fatalf("overview should cover full set: %+v", ov)
	}
}

func TestPostAnalyticsCurrentByPostID(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostAnalyticsRepository(db)

	base := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	mustUpsert(t, repo, "p1", "z-1", 100, 10, 0.10, base)
	mustUpsert(t, repo, "p2", "z-2", 300, 30, 0.20, base)

	cur, err := repo.CurrentByPostID(ctx)
	if err != nil {
		t.Fatalf("current by post: %v", err)
	}
	if len(cur) != 2 {
		t.Fatalf("expected 2 current rows, got %d", len(cur))
	}
	if cur["p1"] == nil || cur["p1"].Impressions != 100 || cur["p2"] == nil || cur["p2"].Impressions != 300 {
		t.Fatalf("current map wrong: %+v", cur)
	}
	// The map is the dedup source: the metric key round-trips.
	if cur["p1"].MetricsKey() != (models.MetricsKey{Impressions: 100, Likes: 10, EngagementRate: 0.10, SyncStatus: "synced"}) {
		t.Fatalf("metric key wrong: %+v", cur["p1"].MetricsKey())
	}
}

func TestListWithPublisherPostID(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	seedPost(t, db, "p1", models.PublisherZernio, "z-1", time.Now().UTC())
	seedPost(t, db, "p2", models.PublisherZernio, "z-2", time.Now().UTC())
	seedPost(t, db, "p3", "", "", time.Now().UTC()) // never published via a publisher

	// A Zernio post that ended up `failed` (a partial publish maps to failed)
	// must STILL be included — it has real per-platform analytics Zernio can
	// return (CON-93 §10). Status is intentionally not a filter.
	if _, err := db.NewInsert().Model(&models.Post{
		ID:              "p4",
		CampaignID:      "camp-1",
		Content:         "content p4",
		MediaURLs:       models.StringSlice{},
		UsedAssetIDs:    models.StringSlice{},
		Status:          models.PostStatusFailed,
		Publisher:       models.PublisherZernio,
		PublisherPostID: "z-4",
		CTAType:         models.CTATypeNone,
		CreatedBy:       "user-1",
	}).Exec(ctx); err != nil {
		t.Fatalf("seed failed post: %v", err)
	}

	// CON-190: a Zernio post owned by a suspended tenant is excluded from the
	// cross-tenant sweep (the query joins tenants and requires status='active').
	// The posts above are in the seeded 'default' tenant, which is active.
	if _, err := db.NewInsert().Model(&models.Tenant{
		ID: "t-sus", Name: "sus", Slug: "t-sus", TierID: models.DefaultTierID,
		Status: models.TenantStatusSuspended, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Exec(ctx); err != nil {
		t.Fatalf("seed suspended tenant: %v", err)
	}
	if _, err := db.NewInsert().Model(&models.Post{
		ID: "p5", CampaignID: "camp-1", Content: "content p5",
		MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: models.PostStatusPublished, Publisher: models.PublisherZernio,
		PublisherPostID: "z-5", CTAType: models.CTATypeNone, CreatedBy: "user-1",
	}).Exec(tenantctx.With(t.Context(), "t-sus")); err != nil {
		t.Fatalf("seed suspended-tenant post: %v", err)
	}

	posts, err := repo.ListWithPublisherPostID(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(posts) != 3 {
		t.Fatalf("got %d want 3", len(posts))
	}
	byID := map[string]string{}
	for _, p := range posts {
		byID[p.ID] = p.PublisherPostID
	}
	if byID["p1"] != "z-1" || byID["p2"] != "z-2" {
		t.Fatalf("match map wrong: %+v", byID)
	}
	if byID["p4"] != "z-4" {
		t.Fatalf("failed (non-published) Zernio post must be included: %+v", byID)
	}
	if _, ok := byID["p3"]; ok {
		t.Fatalf("p3 should be excluded")
	}
	if _, ok := byID["p5"]; ok {
		t.Fatalf("p5 (suspended tenant) must be excluded: %+v", byID)
	}
}

func TestPostAnalyticsUpsertWithSnapshot(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostAnalyticsRepository(db)
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	cur := &models.PostAnalytics{
		PostID: "p1", PublisherPostID: "z-1", Publisher: models.PublisherZernio, Platform: "linkedin",
		Impressions: 100, EngagementRate: 0.1, PlatformAnalytics: models.PlatformAnalyticsList{},
		SyncStatus: "synced", FirstSeenAt: now, LastChangedAt: now, LastCheckedAt: now,
	}
	// One transactional call writes both the current row and its trend point.
	if err := repo.UpsertWithSnapshot(ctx, cur, cur.NewSnapshot(mustID(t), now)); err != nil {
		t.Fatalf("upsert with snapshot: %v", err)
	}
	got, _ := repo.GetByPostID(ctx, "p1")
	if got == nil || got.Impressions != 100 {
		t.Fatalf("current not written: %+v", got)
	}
	curCount, err := db.NewSelect().Model((*models.PostAnalytics)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("current count: %v", err)
	}
	histCount, err := db.NewSelect().Model((*models.PostAnalyticsSnapshot)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("history count: %v", err)
	}
	if curCount != 1 || histCount != 1 {
		t.Fatalf("expected 1 current + 1 history, got %d/%d", curCount, histCount)
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	return id
}

func mustUpsert(t *testing.T, repo repository.PostAnalyticsRepository, postID, pubPostID string, impressions, likes int, rate float64, checkedAt time.Time) {
	t.Helper()
	err := repo.Upsert(tenantCtx(), &models.PostAnalytics{
		PostID:            postID,
		PublisherPostID:   pubPostID,
		Publisher:         models.PublisherZernio,
		Platform:          "linkedin",
		Impressions:       impressions,
		Likes:             likes,
		EngagementRate:    rate,
		PlatformAnalytics: models.PlatformAnalyticsList{},
		SyncStatus:        "synced",
		FirstSeenAt:       checkedAt,
		LastChangedAt:     checkedAt,
		LastCheckedAt:     checkedAt,
	})
	if err != nil {
		t.Fatalf("upsert %s: %v", postID, err)
	}
}
