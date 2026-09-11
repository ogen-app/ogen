package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// These tests use pgtest.MustDB() directly (not openMigratedDB) so triggers fire
// and FKs are enforced — openMigratedDB flips session_replication_role=replica,
// which disables the CON-243 immutability trigger under test.

func TestTierVersionSeedAndReads(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)

	// default-v1 is the seeded internal version (active, not purchasable).
	def, err := vr.LatestActiveByTier(ctx, "default")
	if err != nil {
		t.Fatalf("default latest active: %v", err)
	}
	if def.ID != "ttv-default-v1" || def.Purchasable {
		t.Fatalf("unexpected default v1: %+v", def)
	}

	// The public pricing catalog is active + purchasable = Trial only (Pro/Max
	// are seeded as drafts until pricing is decided).
	cur, err := vr.ListCurrentPurchasable(ctx)
	if err != nil {
		t.Fatalf("list purchasable: %v", err)
	}
	if len(cur) != 1 || cur[0].TierID != "trial" {
		t.Fatalf("expected only the trial tier, got %+v", cur)
	}

	// Trial v1 is free.
	prices, err := vr.PricesByVersion(ctx, "ttv-trial-v1")
	if err != nil {
		t.Fatalf("prices: %v", err)
	}
	if len(prices) != 1 || prices[0].NetMinor != 0 || prices[0].Currency != "EUR" {
		t.Fatalf("unexpected trial price: %+v", prices)
	}

	// Every seeded version's entitlements validate against the catalog.
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, id := range []string{"ttv-default-v1", "ttv-trial-v1", "ttv-pro-v1", "ttv-max-v1"} {
		v, err := vr.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if err := cat.Validate(v.Entitlements); err != nil {
			t.Fatalf("seeded %s entitlements invalid vs catalog: %v", id, err)
		}
	}
}

func TestTierVersionImmutabilityTrigger(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()

	// Updating a published (active) version's PRICE is rejected.
	if _, err := db.NewUpdate().Model((*models.TenantTierVersionPrice)(nil)).
		Set("net_minor = 999").Where("tier_version_id = ?", "ttv-trial-v1").Exec(ctx); err == nil {
		t.Fatal("expected rejection updating an active version's price")
	}

	// Updating a published (active) version row (other than active->retired) is
	// rejected.
	if _, err := db.NewUpdate().Model((*models.TenantTierVersion)(nil)).
		Set("change_reason = 'nope'").Where("id = ?", "ttv-trial-v1").Exec(ctx); err == nil {
		t.Fatal("expected rejection updating an active version")
	}

	// The active -> retired transition succeeds.
	if _, err := db.NewUpdate().Model((*models.TenantTierVersion)(nil)).
		Set("status = 'retired'").Set("retired_at = now()").Where("id = ?", "ttv-trial-v1").Exec(ctx); err != nil {
		t.Fatalf("active->retired should succeed: %v", err)
	}

	// A draft version is freely editable.
	if _, err := db.NewUpdate().Model((*models.TenantTierVersion)(nil)).
		Set("change_reason = 'draft edit ok'").Where("id = ?", "ttv-pro-v1").Exec(ctx); err != nil {
		t.Fatalf("editing a draft should succeed: %v", err)
	}
}

func TestResolverAndBackfill(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()

	vr := repository.NewTenantTierVersionRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)
	tr := repository.NewTenantRepository(db)
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	resolver := entitlements.NewResolver(vr, ar, tr, cat)

	// The seeded 'default' tenant is on the default tier with no assignment yet
	// → the resolver falls back to the tier's latest active version.
	r, err := resolver.Resolve(ctx, models.DefaultTenantID, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve (fallback): %v", err)
	}
	if r.VersionID != "ttv-default-v1" || len(r.Entitlements) == 0 {
		t.Fatalf("unexpected fallback resolution: %+v", r)
	}

	// Dry run counts the default tenant as a candidate but writes nothing.
	dry, err := repository.BackfillTenantTierAssignments(ctx, db, true)
	if err != nil {
		t.Fatalf("backfill dry run: %v", err)
	}
	if dry.Candidates < 1 || dry.Assigned != 0 {
		t.Fatalf("unexpected dry-run report: %+v", dry)
	}

	// Real backfill writes assignments; a second run is a no-op (idempotent).
	rep, err := repository.BackfillTenantTierAssignments(ctx, db, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if rep.Assigned < 1 {
		t.Fatalf("expected at least one assignment, got %+v", rep)
	}
	again, err := repository.BackfillTenantTierAssignments(ctx, db, false)
	if err != nil {
		t.Fatalf("backfill (idempotent): %v", err)
	}
	if again.Assigned != 0 {
		t.Fatalf("second backfill should assign nothing, got %+v", again)
	}

	// The open-ended assignment now covers "now".
	asg, err := ar.CoveringAt(ctx, models.DefaultTenantID, time.Now().UTC())
	if err != nil {
		t.Fatalf("covering assignment: %v", err)
	}
	if asg.Reason != models.AssignmentReasonSignup || asg.ValidTo != nil {
		t.Fatalf("unexpected assignment: %+v", asg)
	}

	// And resolution still lands on default-v1, now via the explicit assignment.
	r2, err := resolver.Resolve(ctx, models.DefaultTenantID, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve (assigned): %v", err)
	}
	if r2.VersionID != "ttv-default-v1" {
		t.Fatalf("expected default-v1 after backfill, got %s", r2.VersionID)
	}
}

func BenchmarkResolverColdPath(b *testing.B) {
	db := pgtest.MustDB()
	ctx := b.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)
	tr := repository.NewTenantRepository(db)
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		b.Fatalf("catalog: %v", err)
	}

	for b.Loop() {
		// A fresh resolver each iteration = a cold cache (the PRD's cold-path
		// benchmark), so every call hits the entitlement tables.
		resolver := entitlements.NewResolver(vr, ar, tr, cat)
		if _, err := resolver.Resolve(ctx, models.DefaultTenantID, time.Now().UTC()); err != nil {
			b.Fatal(err)
		}
	}
}
