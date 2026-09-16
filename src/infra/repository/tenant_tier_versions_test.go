package repository_test

import (
	"database/sql"
	"errors"
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
	// → ResolveCurrent falls back to the tier's latest active version.
	r, err := resolver.ResolveCurrent(ctx, models.DefaultTenantID)
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
	r2, err := resolver.ResolveCurrent(ctx, models.DefaultTenantID)
	if err != nil {
		t.Fatalf("resolve (assigned): %v", err)
	}
	if r2.VersionID != "ttv-default-v1" {
		t.Fatalf("expected default-v1 after backfill, got %s", r2.VersionID)
	}
}

func TestAssignmentAppendOnlyTrigger(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	ar := repository.NewTenantTierAssignmentRepository(db)

	// Give the seeded default tenant an open assignment.
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	a := &models.TenantTierAssignment{ID: id, TenantID: models.DefaultTenantID, TierVersionID: "ttv-default-v1", Reason: models.AssignmentReasonSignup, CreatedAt: time.Now().UTC()}
	if err := ar.Create(ctx, a, time.Now().UTC().Add(-time.Hour), nil); err != nil {
		t.Fatalf("seed open assignment: %v", err)
	}

	// A direct DELETE of live history is rejected (append-only).
	if _, err := db.NewDelete().Model((*models.TenantTierAssignment)(nil)).Where("id = ?", id).Exec(ctx); err == nil {
		t.Fatal("expected append-only rejection deleting an assignment")
	}
	// Rewriting a recorded field is rejected.
	if _, err := db.NewUpdate().Model((*models.TenantTierAssignment)(nil)).
		Set("tier_version_id = ?", "ttv-trial-v1").Where("id = ?", id).Exec(ctx); err == nil {
		t.Fatal("expected append-only rejection rewriting an assignment")
	}
	// Closing the open range (upper infinity -> finite) is the one permitted update.
	if _, err := db.NewUpdate().Model((*models.TenantTierAssignment)(nil)).
		Set("valid = tstzrange(lower(valid), ?, '[)')", time.Now().UTC()).Where("id = ?", id).Exec(ctx); err != nil {
		t.Fatalf("closing an open range should be allowed: %v", err)
	}
}

func TestBackfillClosedHistoryNoOpen(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	tenantRepo := repository.NewTenantRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)

	// A tenant whose only assignment is CLOSED (no open one) — e.g. a future
	// reassignment closed its range but the open successor is missing.
	created := time.Now().UTC().Add(-72 * time.Hour)
	tn := &models.Tenant{ID: "tn-closed", Name: "Closed", Slug: "closed-history", TierID: models.DefaultTierID, CreatedAt: created, UpdatedAt: created}
	if err := tenantRepo.Create(ctx, tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	// Truncate to microseconds: Postgres timestamptz has microsecond precision,
	// so the value read back through the range's upper bound is truncated.
	closedEnd := created.Add(24 * time.Hour).Truncate(time.Microsecond)
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	closed := &models.TenantTierAssignment{ID: id, TenantID: tn.ID, TierVersionID: "ttv-default-v1", Reason: models.AssignmentReasonUpgrade, CreatedAt: time.Now().UTC()}
	if err := ar.Create(ctx, closed, created, &closedEnd); err != nil {
		t.Fatalf("seed closed assignment: %v", err)
	}

	// The naive [created_at, infinity) insert overlaps the closed range; the
	// backfill must NOT silently skip on the exclusion violation — it resumes the
	// open range at the end of the closed one, leaving exactly one open assignment.
	if _, err := repository.BackfillTenantTierAssignments(ctx, db, false); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	open, err := ar.CoveringAt(ctx, tn.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("expected an open assignment covering now: %v", err)
	}
	if open.ValidTo != nil {
		t.Fatalf("expected an open-ended assignment, got %+v", open)
	}
	if open.ValidFrom == nil || !open.ValidFrom.Equal(closedEnd) {
		t.Fatalf("expected open range to start at the closed range's end %v, got %+v", closedEnd, open.ValidFrom)
	}
}

func TestTierVersionDeleteDraft(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)

	// A seeded DRAFT (ttv-pro-v1) deletes cleanly; its price rows (none here) cascade.
	if err := vr.DeleteDraft(ctx, "ttv-pro-v1"); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if _, err := vr.GetByID(ctx, "ttv-pro-v1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("draft still present after delete: err = %v", err)
	}

	// A published (active) version cannot be deleted.
	if err := vr.DeleteDraft(ctx, "ttv-trial-v1"); !errors.Is(err, repository.ErrVersionNotDraft) {
		t.Fatalf("delete active version: err = %v, want ErrVersionNotDraft", err)
	}
	// A missing version is NotFound.
	if err := vr.DeleteDraft(ctx, "ttv-nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delete missing version: err = %v, want sql.ErrNoRows", err)
	}
}

func TestTierVersionGuardedRetire(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)
	now := time.Now().UTC()

	// Stand up a second ACTIVE version of the default tier (v2) by
	// creating a draft then publishing it — the reassignment target.
	next, err := vr.NextVersion(ctx, "default")
	if err != nil {
		t.Fatalf("next version: %v", err)
	}
	v2ID, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	v2 := &models.TenantTierVersion{ID: v2ID, TierID: "default", Version: next, Status: models.TierVersionStatusDraft, Entitlements: map[string]any{}, CreatedAt: now}
	if err := vr.Create(ctx, v2, nil); err != nil {
		t.Fatalf("create v2 draft: %v", err)
	}
	if err := vr.Publish(ctx, v2ID, "second active version", now); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	// Put the default tenant on default-v1 (open assignment an hour ago).
	aID, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	open := &models.TenantTierAssignment{ID: aID, TenantID: models.DefaultTenantID, TierVersionID: "ttv-default-v1", Reason: models.AssignmentReasonSignup, CreatedAt: now}
	if err := ar.Create(ctx, open, now.Add(-time.Hour), nil); err != nil {
		t.Fatalf("seed open assignment: %v", err)
	}

	// Retiring default-v1 with a live assignment and neither flag is refused.
	if _, err := vr.Retire(ctx, "ttv-default-v1", repository.RetireOptions{}, now); !errors.Is(err, repository.ErrVersionHasLiveAssignments) {
		t.Fatalf("retire blocked: err = %v, want ErrVersionHasLiveAssignments", err)
	}
	// Reassigning to itself / a draft / a missing version is rejected.
	if _, err := vr.Retire(ctx, "ttv-default-v1", repository.RetireOptions{ReassignToVersionID: "ttv-default-v1"}, now); !errors.Is(err, repository.ErrReassignTargetIsSelf) {
		t.Fatalf("reassign to self: err = %v, want ErrReassignTargetIsSelf", err)
	}
	if _, err := vr.Retire(ctx, "ttv-default-v1", repository.RetireOptions{ReassignToVersionID: "ttv-max-v1"}, now); !errors.Is(err, repository.ErrReassignTargetNotActive) {
		t.Fatalf("reassign to draft: err = %v, want ErrReassignTargetNotActive", err)
	}
	if _, err := vr.Retire(ctx, "ttv-default-v1", repository.RetireOptions{ReassignToVersionID: "ttv-nope"}, now); !errors.Is(err, repository.ErrReassignTargetNotFound) {
		t.Fatalf("reassign to missing: err = %v, want ErrReassignTargetNotFound", err)
	}

	// TenantsOnVersion lists the blocking tenant before we act.
	on, err := vr.TenantsOnVersion(ctx, "ttv-default-v1", 10, 0)
	if err != nil {
		t.Fatalf("tenants on version: %v", err)
	}
	if len(on) != 1 || on[0].TenantID != models.DefaultTenantID {
		t.Fatalf("unexpected tenants on default-v1: %+v", on)
	}

	// Reassign-then-retire: migrate the tenant onto v2 and retire v1 atomically.
	reassigned, err := vr.Retire(ctx, "ttv-default-v1", repository.RetireOptions{ReassignToVersionID: v2ID}, now)
	if err != nil {
		t.Fatalf("retire w/ reassign: %v", err)
	}
	if reassigned != 1 {
		t.Fatalf("reassigned = %d, want 1", reassigned)
	}
	// v1 is retired and holds no more live assignments.
	v1, err := vr.GetByID(ctx, "ttv-default-v1")
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1.Status != models.TierVersionStatusRetired || v1.RetiredAt == nil {
		t.Fatalf("v1 not retired: %+v", v1)
	}
	if n, _ := vr.OpenAssignmentCount(ctx, "ttv-default-v1"); n != 0 {
		t.Fatalf("v1 still has %d live assignments after reassign-retire", n)
	}
	// The tenant now sits open on v2.
	cover, err := ar.CoveringAt(ctx, models.DefaultTenantID, now)
	if err != nil {
		t.Fatalf("covering after reassign: %v", err)
	}
	if cover.TierVersionID != v2ID || cover.ValidTo != nil || cover.Reason != models.AssignmentReasonOperatorSet {
		t.Fatalf("unexpected assignment after reassign: %+v", cover)
	}
	if n, _ := vr.OpenAssignmentCount(ctx, v2ID); n != 1 {
		t.Fatalf("v2 open assignments = %d, want 1", n)
	}
}

func TestTierVersionForceRetireGrandfathers(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)
	now := time.Now().UTC()

	aID, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	open := &models.TenantTierAssignment{ID: aID, TenantID: models.DefaultTenantID, TierVersionID: "ttv-default-v1", Reason: models.AssignmentReasonSignup, CreatedAt: now}
	if err := ar.Create(ctx, open, now.Add(-time.Hour), nil); err != nil {
		t.Fatalf("seed open assignment: %v", err)
	}

	// Force retire leaves the tenant grandfathered on the now-retired version.
	reassigned, err := vr.Retire(ctx, "ttv-default-v1", repository.RetireOptions{Force: true}, now)
	if err != nil {
		t.Fatalf("force retire: %v", err)
	}
	if reassigned != 0 {
		t.Fatalf("reassigned = %d, want 0 on force", reassigned)
	}
	if n, _ := vr.OpenAssignmentCount(ctx, "ttv-default-v1"); n != 1 {
		t.Fatalf("force retire should keep the live assignment, got %d", n)
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
		if _, err := resolver.ResolveCurrent(ctx, models.DefaultTenantID); err != nil {
			b.Fatal(err)
		}
	}
}
