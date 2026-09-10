package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// openClassificationDB returns a fresh, fully-migrated DB with FK enforcement
// ON — the CON-208 tables lean on FK RESTRICT / cascade / violation behaviour,
// so (unlike openMigratedDB) it must NOT disable foreign keys.
func openClassificationDB(t *testing.T) *bun.DB {
	t.Helper()
	db := pgtest.MustDB()
	db.DB.SetMaxOpenConns(2)
	db.DB.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mintID fails the test rather than returning an error, keeping call sites terse.
func mintID(t *testing.T) string {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	return id
}

// seedTenant inserts a tenant on the given tier (slug = id keeps it unique).
func seedTenant(t *testing.T, db *bun.DB, id, name, tierID string) {
	t.Helper()
	now := time.Now().UTC()
	tn := &models.Tenant{ID: id, Name: name, Slug: id, TierID: tierID, CreatedAt: now, UpdatedAt: now}
	if _, err := db.NewInsert().Model(tn).Exec(context.Background()); err != nil {
		t.Fatalf("seed tenant %s: %v", id, err)
	}
}

func sqlState(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}
	return ""
}

// TestTenantTierRepositoryCRUD covers the tier catalog: create (with the
// name-uniqueness clash), read, update, delete, and the ON DELETE RESTRICT that
// protects an in-use tier.
func TestTenantTierRepositoryCRUD(t *testing.T) {
	db := openClassificationDB(t)
	ctx := context.Background()
	repo := repository.NewTenantTierRepository(db)

	// The migration seeds the 'default' tier.
	got, err := repo.GetByID(ctx, models.DefaultTierID)
	if err != nil {
		t.Fatalf("get default tier: %v", err)
	}
	if got.Name != "Default" {
		t.Fatalf("default tier name = %q, want Default", got.Name)
	}

	gold := &models.TenantTier{ID: mintID(t), Name: "Gold", Color: "#ffd700", Description: "Top tier", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.Create(ctx, gold); err != nil {
		t.Fatalf("create gold: %v", err)
	}

	// Name uniqueness clash surfaces SQLSTATE 23505.
	dup := &models.TenantTier{ID: mintID(t), Name: "Gold", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.Create(ctx, dup); sqlState(err) != "23505" {
		t.Fatalf("duplicate name: err = %v (state %q), want 23505", err, sqlState(err))
	}

	// Update replaces name/color/description and bumps updated_at.
	gold.Name, gold.Color, gold.Description = "Gold Plus", "#ffdf00", "Even better"
	if err := repo.Update(ctx, gold); err != nil {
		t.Fatalf("update gold: %v", err)
	}
	reloaded, err := repo.GetByID(ctx, gold.ID)
	if err != nil {
		t.Fatalf("get updated gold: %v", err)
	}
	if reloaded.Name != "Gold Plus" || reloaded.Color != "#ffdf00" || reloaded.Description != "Even better" {
		t.Fatalf("update not persisted: %+v", reloaded)
	}
	if !reloaded.UpdatedAt.After(reloaded.CreatedAt) {
		t.Fatalf("updated_at %v not after created_at %v", reloaded.UpdatedAt, reloaded.CreatedAt)
	}

	// Update of an unknown id → sql.ErrNoRows.
	if err := repo.Update(ctx, &models.TenantTier{ID: "nope", Name: "X"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("update missing: err = %v, want sql.ErrNoRows", err)
	}

	// In-use tier can't be deleted (ON DELETE RESTRICT → 23503).
	seedTenant(t, db, mintID(t), "Acme", gold.ID)
	if _, err := repo.Delete(ctx, gold.ID); sqlState(err) != "23503" {
		t.Fatalf("delete in-use tier: err = %v (state %q), want 23503", err, sqlState(err))
	}

	// A free tier deletes; deleting a missing id reports false.
	free := &models.TenantTier{ID: mintID(t), Name: "Free", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.Create(ctx, free); err != nil {
		t.Fatalf("create free: %v", err)
	}
	deleted, err := repo.Delete(ctx, free.ID)
	if err != nil || !deleted {
		t.Fatalf("delete free: deleted=%v err=%v", deleted, err)
	}
	if deleted, _ := repo.Delete(ctx, "nope"); deleted {
		t.Fatalf("delete missing tier reported true")
	}
}

// TestTenantGroupRepositoryAndMembership covers the group catalog plus the
// many-to-many membership: idempotent add, remove, cascade on group delete, and
// the FK violation for an unknown tenant/group.
func TestTenantGroupRepositoryAndMembership(t *testing.T) {
	db := openClassificationDB(t)
	ctx := context.Background()
	repo := repository.NewTenantGroupRepository(db)

	beta := &models.TenantGroup{ID: mintID(t), Name: "Beta", Color: "#5e6ad2", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	vip := &models.TenantGroup{ID: mintID(t), Name: "VIP", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	for _, g := range []*models.TenantGroup{beta, vip} {
		if err := repo.Create(ctx, g); err != nil {
			t.Fatalf("create group %s: %v", g.Name, err)
		}
	}

	tenantID := mintID(t)
	seedTenant(t, db, tenantID, "Acme", models.DefaultTierID)

	// Add is idempotent.
	if err := repo.AddTenant(ctx, tenantID, beta.ID); err != nil {
		t.Fatalf("add beta: %v", err)
	}
	if err := repo.AddTenant(ctx, tenantID, beta.ID); err != nil {
		t.Fatalf("re-add beta (should be idempotent): %v", err)
	}
	if err := repo.AddTenant(ctx, tenantID, vip.ID); err != nil {
		t.Fatalf("add vip: %v", err)
	}

	groups, err := repo.ListByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("list by tenant: %v", err)
	}
	if len(groups) != 2 || groups[0].Name != "Beta" || groups[1].Name != "VIP" {
		t.Fatalf("membership = %+v, want [Beta VIP] ordered by name", groups)
	}

	// Remove is idempotent.
	if ok, err := repo.RemoveTenant(ctx, tenantID, beta.ID); err != nil || !ok {
		t.Fatalf("remove beta: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.RemoveTenant(ctx, tenantID, beta.ID); err != nil || ok {
		t.Fatalf("re-remove beta: ok=%v err=%v, want ok=false", ok, err)
	}

	// Unknown tenant / group fail the FK (23503).
	if err := repo.AddTenant(ctx, "ghost", vip.ID); sqlState(err) != "23503" {
		t.Fatalf("add unknown tenant: err = %v (state %q), want 23503", err, sqlState(err))
	}
	if err := repo.AddTenant(ctx, tenantID, "ghost"); sqlState(err) != "23503" {
		t.Fatalf("add to unknown group: err = %v (state %q), want 23503", err, sqlState(err))
	}

	// Deleting a group cascades its assignments away.
	if ok, err := repo.Delete(ctx, vip.ID); err != nil || !ok {
		t.Fatalf("delete vip: ok=%v err=%v", ok, err)
	}
	remaining, err := repo.ListByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("list after cascade: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("membership after group delete = %+v, want empty (cascaded)", remaining)
	}
}

// TestTenantClassificationHydrationAndFilters covers the tenant admin read/write
// paths: hydration of tier+groups, tier/group filtering, paging + total, tier
// reassignment, and (CON-190) all-status reads with an optional status filter.
func TestTenantClassificationHydrationAndFilters(t *testing.T) {
	db := openClassificationDB(t)
	ctx := context.Background()
	tenantRepo := repository.NewTenantRepository(db)
	tierRepo := repository.NewTenantTierRepository(db)
	groupRepo := repository.NewTenantGroupRepository(db)

	pro := &models.TenantTier{ID: mintID(t), Name: "Pro", Color: "#00b3a4", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := tierRepo.Create(ctx, pro); err != nil {
		t.Fatalf("create pro tier: %v", err)
	}
	beta := &models.TenantGroup{ID: mintID(t), Name: "Beta", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	vip := &models.TenantGroup{ID: mintID(t), Name: "VIP", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	for _, g := range []*models.TenantGroup{beta, vip} {
		if err := groupRepo.Create(ctx, g); err != nil {
			t.Fatalf("create group %s: %v", g.Name, err)
		}
	}

	t1, t2, t3 := mintID(t), mintID(t), mintID(t)
	seedTenant(t, db, t1, "T1", pro.ID)
	seedTenant(t, db, t2, "T2", models.DefaultTierID)
	seedTenant(t, db, t3, "T3", pro.ID)
	mustAdd := func(tenantID, groupID string) {
		if err := groupRepo.AddTenant(ctx, tenantID, groupID); err != nil {
			t.Fatalf("add %s→%s: %v", tenantID, groupID, err)
		}
	}
	mustAdd(t1, beta.ID)
	mustAdd(t2, beta.ID)
	mustAdd(t2, vip.ID)

	// GetByIDWithClassification hydrates tier + groups.
	got, err := tenantRepo.GetByIDWithClassification(ctx, t2)
	if err != nil {
		t.Fatalf("get t2: %v", err)
	}
	if got.Tier == nil || got.Tier.Name != "Default" {
		t.Fatalf("t2 tier = %+v, want Default", got.Tier)
	}
	if len(got.Groups) != 2 || got.Groups[0].Name != "Beta" || got.Groups[1].Name != "VIP" {
		t.Fatalf("t2 groups = %+v, want [Beta VIP]", got.Groups)
	}
	// A tenant with no groups hydrates to an empty (non-nil) slice.
	g3, err := tenantRepo.GetByIDWithClassification(ctx, t3)
	if err != nil {
		t.Fatalf("get t3: %v", err)
	}
	if g3.Groups == nil || len(g3.Groups) != 0 {
		t.Fatalf("t3 groups = %+v, want non-nil empty slice", g3.Groups)
	}
	if _, err := tenantRepo.GetByIDWithClassification(ctx, "ghost"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get unknown tenant: err = %v, want sql.ErrNoRows", err)
	}

	// Filter by tier: T1 + T3 (Pro), not T2.
	proTenants, total, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{TierID: pro.ID})
	if err != nil {
		t.Fatalf("list by tier: %v", err)
	}
	if got := idSet(proTenants); !got[t1] || !got[t3] || got[t2] || total != 2 {
		t.Fatalf("pro tenants = %v total=%d, want {t1,t3} total 2", got, total)
	}
	// Each is hydrated.
	if proTenants[0].Tier == nil || proTenants[0].Tier.Name != "Pro" {
		t.Fatalf("listed tenant not hydrated: %+v", proTenants[0].Tier)
	}

	// Filter by group: VIP → only T2.
	vipTenants, _, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{GroupID: vip.ID})
	if err != nil {
		t.Fatalf("list by group: %v", err)
	}
	if len(vipTenants) != 1 || vipTenants[0].ID != t2 {
		t.Fatalf("vip tenants = %v, want [t2]", idSet(vipTenants))
	}

	// Combined tier+group filter: Pro AND Beta → only T1.
	combo, _, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{TierID: pro.ID, GroupID: beta.ID})
	if err != nil {
		t.Fatalf("list combo: %v", err)
	}
	if len(combo) != 1 || combo[0].ID != t1 {
		t.Fatalf("combo = %v, want [t1]", idSet(combo))
	}

	// Paging: limit 1 returns one row but the full match total.
	page, total, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{TierID: pro.ID, Limit: 1})
	if err != nil {
		t.Fatalf("list paged: %v", err)
	}
	if len(page) != 1 || total != 2 {
		t.Fatalf("paged: len=%d total=%d, want len 1 total 2", len(page), total)
	}

	// SetTier reassigns; unknown tenant → false; unknown tier → FK violation.
	if ok, err := tenantRepo.SetTier(ctx, t1, models.DefaultTierID); err != nil || !ok {
		t.Fatalf("set tier: ok=%v err=%v", ok, err)
	}
	if after, _ := tenantRepo.GetByIDWithClassification(ctx, t1); after.Tier == nil || after.Tier.Name != "Default" {
		t.Fatalf("t1 tier after reassign = %+v, want Default", after.Tier)
	}
	if ok, err := tenantRepo.SetTier(ctx, "ghost", models.DefaultTierID); err != nil || ok {
		t.Fatalf("set tier unknown tenant: ok=%v err=%v, want ok=false", ok, err)
	}
	if _, err := tenantRepo.SetTier(ctx, t2, "ghost-tier"); sqlState(err) != "23503" {
		t.Fatalf("set unknown tier: err = %v (state %q), want 23503", err, sqlState(err))
	}

	// CON-190: soft-deleting sets status='deleted' alongside deleted_at. The
	// operator admin reads now surface tenants of ANY status (so a deleted tenant
	// stays inspectable and restorable); the status filter is how a caller narrows.
	if _, err := db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("deleted_at = ?", time.Now().UTC()).
		Set("status = ?", models.TenantStatusDeleted).
		Where("id = ?", t3).Exec(ctx); err != nil {
		t.Fatalf("soft-delete t3: %v", err)
	}
	// GetByIDWithClassification returns the deleted tenant (any status), not ErrNoRows.
	if g, err := tenantRepo.GetByIDWithClassification(ctx, t3); err != nil || g.Status != models.TenantStatusDeleted {
		t.Fatalf("get soft-deleted t3: got %+v err = %v, want status=deleted", g, err)
	}
	// The unfiltered list still includes the deleted tenant (all statuses by default).
	all, _, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{TierID: pro.ID})
	if err != nil {
		t.Fatalf("list after soft-delete: %v", err)
	}
	if !idSet(all)[t3] {
		t.Fatalf("deleted t3 should still be listed by default (all statuses): %v", idSet(all))
	}
	// Filtering by status='active' excludes the deleted tenant.
	active, _, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{TierID: pro.ID, Status: models.TenantStatusActive})
	if err != nil {
		t.Fatalf("list active after soft-delete: %v", err)
	}
	if idSet(active)[t3] {
		t.Fatalf("deleted t3 must be excluded when filtering status=active: %v", idSet(active))
	}
}

// TestTenantSetStatusIdempotent guards CON-190: SetStatus is an atomic
// read-modify-write. Setting a tenant to the status it already has is a success
// no-op that preserves deleted_at + updated_at — so a repeated delete keeps the
// ORIGINAL deletion timestamp rather than resetting it — and an unknown id is a
// distinct not-found (false), not a silent success.
func TestTenantSetStatusIdempotent(t *testing.T) {
	db := openClassificationDB(t)
	ctx := context.Background()
	repo := repository.NewTenantRepository(db)

	id := mintID(t)
	seedTenant(t, db, id, "Acme", models.DefaultTierID)

	// First delete stamps deleted_at = at0.
	at0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if ok, err := repo.SetStatus(ctx, id, models.TenantStatusDeleted, "", at0); err != nil || !ok {
		t.Fatalf("first delete: ok=%v err=%v, want true/nil", ok, err)
	}
	tn, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if tn.Status != models.TenantStatusDeleted || tn.DeletedAt == nil || !tn.DeletedAt.Equal(at0) {
		t.Fatalf("after delete: status=%q deleted_at=%v, want deleted @ %v", tn.Status, tn.DeletedAt, at0)
	}

	// A repeated delete at a LATER time is a success no-op: deleted_at + updated_at
	// stay pinned to the first delete.
	at1 := at0.Add(time.Hour)
	if ok, err := repo.SetStatus(ctx, id, models.TenantStatusDeleted, "", at1); err != nil || !ok {
		t.Fatalf("repeated delete: ok=%v err=%v, want true/nil", ok, err)
	}
	tn2, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get after repeated delete: %v", err)
	}
	if tn2.DeletedAt == nil || !tn2.DeletedAt.Equal(at0) {
		t.Fatalf("repeated delete moved deleted_at to %v, want unchanged %v", tn2.DeletedAt, at0)
	}
	if !tn2.UpdatedAt.Equal(tn.UpdatedAt) {
		t.Fatalf("repeated delete bumped updated_at to %v, want unchanged %v", tn2.UpdatedAt, tn.UpdatedAt)
	}

	// An unknown id is a distinct not-found (false), never a silent success.
	if ok, err := repo.SetStatus(ctx, "ghost", models.TenantStatusSuspended, "why", at1); err != nil || ok {
		t.Fatalf("unknown tenant: ok=%v err=%v, want false/nil", ok, err)
	}

	// Restore clears deleted_at and flips status back (the invariant holds).
	if ok, err := repo.SetStatus(ctx, id, models.TenantStatusActive, "", at1); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v, want true/nil", ok, err)
	}
	tn3, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get after restore: %v", err)
	}
	if tn3.Status != models.TenantStatusActive || tn3.DeletedAt != nil {
		t.Fatalf("after restore: status=%q deleted_at=%v, want active / nil", tn3.Status, tn3.DeletedAt)
	}
}

// TestTenantListPagingDeterministicOnTiedCreatedAt locks in the (created_at, id)
// tiebreak: when created_at ties, limit/offset paging must be a total order —
// every row exactly once, ascending by id — not arbitrary (which would drop or
// repeat rows across pages).
func TestTenantListPagingDeterministicOnTiedCreatedAt(t *testing.T) {
	db := openClassificationDB(t)
	ctx := context.Background()
	tenantRepo := repository.NewTenantRepository(db)
	tierRepo := repository.NewTenantTierRepository(db)

	tie := &models.TenantTier{ID: mintID(t), Name: "Tie", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := tierRepo.Create(ctx, tie); err != nil {
		t.Fatalf("create tie tier: %v", err)
	}
	// Three tenants sharing one created_at, so only the id tiebreak orders them.
	ts := time.Now().UTC().Truncate(time.Second)
	ids := []string{mintID(t), mintID(t), mintID(t)}
	for _, id := range ids {
		tn := &models.Tenant{ID: id, Name: id, Slug: id, TierID: tie.ID, CreatedAt: ts, UpdatedAt: ts}
		if _, err := db.NewInsert().Model(tn).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	slices.Sort(ids) // expected page order: ascending by id

	var got []string
	for off := range 3 {
		page, total, err := tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{TierID: tie.ID, Limit: 1, Offset: off})
		if err != nil {
			t.Fatalf("page off=%d: %v", off, err)
		}
		if total != 3 || len(page) != 1 {
			t.Fatalf("page off=%d: len=%d total=%d, want len 1 total 3", off, len(page), total)
		}
		got = append(got, page[0].ID)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("paged order = %v, want ascending-by-id %v", got, ids)
		}
	}
}

func idSet(tenants []models.Tenant) map[string]bool {
	m := make(map[string]bool, len(tenants))
	for _, t := range tenants {
		m[t.ID] = true
	}
	return m
}
