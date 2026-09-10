package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// seedUser inserts a user with an explicit tenant_id. Users are not TenantScoped,
// so there is no hook to stamp the tenant — the model field is used verbatim.
// openMigratedDB bypasses FK enforcement, so a synthetic tenant_id needs no
// tenants row.
func seedUser(t *testing.T, db *bun.DB, id, tenantID, email string) {
	t.Helper()
	u := &models.User{
		ID: id, AccountID: id, TenantID: tenantID, Name: "User " + id, Email: email,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := db.NewInsert().Model(u).Exec(t.Context()); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// TestUserRepositoryTenantIsolation guards CON-97: the User model is deliberately
// not TenantScoped, so the repository must apply the tenant filter by hand.
// Without it, GET /api/users (List) and GET /api/users/:id (GetByID) leak users
// across tenants.
func TestUserRepositoryTenantIsolation(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewUserRepository(db)

	ctxA := tenantctx.With(t.Context(), "ta")

	seedUser(t, db, "u-a", "ta", "a@example.com")
	seedUser(t, db, "u-b", "tb", "b@example.com")

	// List returns only the caller tenant's users — never tenant tb's.
	usersA, err := repo.List(ctxA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(usersA) != 1 || usersA[0].ID != "u-a" {
		t.Fatalf("tenant ta must see only its own user, got %+v", usersA)
	}

	// GetByID cannot reach another tenant's user even with a valid id.
	if _, err := repo.GetByID(ctxA, "u-b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant GetByID must fail closed with ErrNoRows, got %v", err)
	}
	if got, err := repo.GetByID(ctxA, "u-a"); err != nil || got.ID != "u-a" {
		t.Fatalf("in-tenant GetByID should return the user, got %+v err=%v", got, err)
	}

	// A non-system context with no tenant must fail closed rather than return
	// every tenant's users.
	if _, err := repo.List(t.Context()); !errors.Is(err, tenantctx.ErrNoTenant) {
		t.Fatalf("List without a tenant must fail closed, got %v", err)
	}
}

// TestUserRepositoryGetByAccountIDTieBreak guards CON-147: GetByAccountID chooses
// the account's default workspace, so it must be deterministic when two
// memberships share a created_at (e.g. seeded in one transaction). The id
// tie-breaker makes the smallest-id membership win regardless of scan order.
func TestUserRepositoryGetByAccountIDTieBreak(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewUserRepository(db)
	ctx := t.Context()

	// GetByAccountID joins tenants and filters deleted_at, so the memberships need
	// real, live tenants rows to resolve to.
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := db.NewInsert().Model(&models.Tenant{ID: id, Name: id, Slug: id, TierID: models.DefaultTierID, CreatedAt: ts, UpdatedAt: ts}).Exec(ctx); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
	}

	// One account, two memberships with the SAME created_at. Insert the larger id
	// first so a missing tie-breaker would be free to return it (scan/heap order).
	same := time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)
	for _, m := range []*models.User{
		{ID: "u-zzz", AccountID: "acc", TenantID: "t-1", Name: "Z", Email: "e@x", CreatedAt: same, UpdatedAt: same},
		{ID: "u-aaa", AccountID: "acc", TenantID: "t-2", Name: "A", Email: "e@x", CreatedAt: same, UpdatedAt: same},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed membership %s: %v", m.ID, err)
		}
	}

	got, err := repo.GetByAccountID(ctx, "acc")
	if err != nil {
		t.Fatalf("GetByAccountID: %v", err)
	}
	if got.ID != "u-aaa" {
		t.Fatalf("tie on created_at must resolve to the smallest id (u-aaa), got %s", got.ID)
	}
}

// TestUserRepositoryMembershipStatusGate guards CON-190: membership resolution is
// the consequence chokepoint, so a non-active tenant (suspended or deleted) is
// unreachable — GetMembership resolves it to ErrNoRows and GetByAccountID falls
// back past it, exactly like a soft-deleted workspace.
func TestUserRepositoryMembershipStatusGate(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewUserRepository(db)
	ctx := t.Context()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedTenant := func(id, status string) {
		tn := &models.Tenant{ID: id, Name: id, Slug: id, TierID: models.DefaultTierID, Status: status, CreatedAt: ts, UpdatedAt: ts}
		if _, err := db.NewInsert().Model(tn).Exec(ctx); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
	}
	seedTenant("t-active", models.TenantStatusActive)
	seedTenant("t-suspended", models.TenantStatusSuspended)

	// One account, member of both. The suspended membership is older AND inserted
	// first, so a missing gate would let GetByAccountID return it.
	for _, m := range []*models.User{
		{ID: "u-sus", AccountID: "acc", TenantID: "t-suspended", Name: "S", Email: "e@x", CreatedAt: ts, UpdatedAt: ts},
		{ID: "u-act", AccountID: "acc", TenantID: "t-active", Name: "A", Email: "e@x", CreatedAt: ts.Add(time.Hour), UpdatedAt: ts},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed membership %s: %v", m.ID, err)
		}
	}

	// GetMembership: the active workspace resolves; the suspended one is ErrNoRows.
	if got, err := repo.GetMembership(ctx, "acc", "t-active"); err != nil || got.ID != "u-act" {
		t.Fatalf("GetMembership active: got %+v err=%v, want u-act", got, err)
	}
	if _, err := repo.GetMembership(ctx, "acc", "t-suspended"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetMembership suspended must fail closed with ErrNoRows, got %v", err)
	}

	// GetByAccountID skips the (older) suspended workspace and lands on the active one.
	if got, err := repo.GetByAccountID(ctx, "acc"); err != nil || got.ID != "u-act" {
		t.Fatalf("GetByAccountID must skip the suspended workspace, got %+v err=%v", got, err)
	}
}
