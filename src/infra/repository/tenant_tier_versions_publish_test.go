package repository_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// createDraft adds a draft version of a seeded tier and returns its id.
func createDraft(t *testing.T, vr repository.TenantTierVersionRepository, tierID string, purchasable bool) string {
	t.Helper()
	ctx := t.Context()
	n, err := vr.NextVersion(ctx, tierID)
	if err != nil {
		t.Fatalf("next version: %v", err)
	}
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	v := &models.TenantTierVersion{ID: id, TierID: tierID, Version: n, Status: models.TierVersionStatusDraft, Purchasable: purchasable, Entitlements: map[string]any{}, CreatedAt: time.Now().UTC()}
	if err := vr.Create(ctx, v, nil); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return id
}

func versionStatus(t *testing.T, vr repository.TenantTierVersionRepository, id string) string {
	t.Helper()
	v, err := vr.GetByID(t.Context(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return v.Status
}

func TestTierVersionPublishOneActivePurchasable(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	now := time.Now().UTC()

	// trial-v1 is on sale, so a second purchasable trial version is refused
	// and names v1.
	clashing := createDraft(t, vr, "trial", true)
	_, err := vr.Publish(ctx, clashing, "second on sale", repository.SupersedeOptions{}, now)
	clash, ok := errors.AsType[*repository.PurchasableVersionExistsError](err)
	if !ok || clash.Version != 1 || clash.VersionID != "ttv-trial-v1" || !errors.Is(err, repository.ErrPurchasableVersionExists) {
		t.Fatalf("publish beside trial-v1: err = %v, want PurchasableVersionExistsError for v1", err)
	}
	if got := versionStatus(t, vr, clashing); got != models.TierVersionStatusDraft {
		t.Fatalf("refused draft status = %q, want draft", got)
	}

	// A non-purchasable version publishes beside it.
	hidden := createDraft(t, vr, "trial", false)
	if _, err := vr.Publish(ctx, hidden, "internal variant", repository.SupersedeOptions{}, now); err != nil {
		t.Fatalf("publish non-purchasable: %v", err)
	}

	// Once trial-v1 is retired, a purchasable version publishes: a retired
	// version is no longer on sale.
	if _, err := vr.Retire(ctx, "ttv-trial-v1", repository.RetireOptions{Force: true}, now); err != nil {
		t.Fatalf("retire trial-v1: %v", err)
	}
	if _, err := vr.Publish(ctx, clashing, "replaces retired v1", repository.SupersedeOptions{}, now); err != nil {
		t.Fatalf("publish after retire: %v", err)
	}

	// The unique index holds even when the check is bypassed.
	backdoor := createDraft(t, vr, "trial", true)
	_, err = db.NewUpdate().Model((*models.TenantTierVersion)(nil)).
		Set("status = ?", models.TierVersionStatusActive).
		Set("published_at = ?", now).
		Where("id = ?", backdoor).Exec(ctx)
	if err == nil {
		t.Fatal("direct update to a second active purchasable version succeeded, want unique violation")
	}
}

func TestTierVersionPublishConcurrentPurchasable(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	now := time.Now().UTC()

	if _, err := vr.Retire(ctx, "ttv-trial-v1", repository.RetireOptions{Force: true}, now); err != nil {
		t.Fatalf("retire trial-v1: %v", err)
	}
	ids := []string{createDraft(t, vr, "trial", true), createDraft(t, vr, "trial", true)}

	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			_, errs[i] = vr.Publish(ctx, id, "race", repository.SupersedeOptions{}, now)
		})
	}
	wg.Wait()

	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, repository.ErrPurchasableVersionExists):
			t.Fatalf("losing publish: err = %v, want ErrPurchasableVersionExists", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent publishes succeeded, want exactly 1 (errs = %v)", won, errs)
	}
}

func TestTierVersionPublishSupersedeReassigns(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)
	now := time.Now().UTC()

	putOnVersion(t, ar, "ttv-trial-v1", now.Add(-time.Hour))
	next := createDraft(t, vr, "trial", true)

	// Bad targets are refused before anything changes.
	for _, tc := range []struct {
		name string
		opts repository.SupersedeOptions
		want error
	}{
		{"self", repository.SupersedeOptions{VersionID: next, Force: true}, repository.ErrSupersedeTargetIsSelf},
		{"missing", repository.SupersedeOptions{VersionID: "ttv-nope", Force: true}, repository.ErrSupersedeTargetNotFound},
		{"other tier", repository.SupersedeOptions{VersionID: "ttv-default-v1", Force: true}, repository.ErrSupersedeTargetOtherTier},
		{"live tenants, no flag", repository.SupersedeOptions{VersionID: "ttv-trial-v1"}, repository.ErrVersionHasLiveAssignments},
	} {
		if _, err := vr.Publish(ctx, next, "x", tc.opts, now); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if versionStatus(t, vr, next) != models.TierVersionStatusDraft || versionStatus(t, vr, "ttv-trial-v1") != models.TierVersionStatusActive {
		t.Fatal("a refused supersede changed a version's status")
	}

	reassigned, err := vr.Publish(ctx, next, "trial v2", repository.SupersedeOptions{VersionID: "ttv-trial-v1", ReassignToPublished: true}, now)
	if err != nil {
		t.Fatalf("publish superseding trial-v1: %v", err)
	}
	if reassigned != 1 {
		t.Fatalf("reassigned = %d, want 1", reassigned)
	}
	if versionStatus(t, vr, "ttv-trial-v1") != models.TierVersionStatusRetired || versionStatus(t, vr, next) != models.TierVersionStatusActive {
		t.Fatal("supersede did not retire v1 and activate v2")
	}
	if n, _ := vr.OpenAssignmentCount(ctx, "ttv-trial-v1"); n != 0 {
		t.Fatalf("trial-v1 still has %d live assignments", n)
	}
	if n, _ := vr.OpenAssignmentCount(ctx, next); n != 1 {
		t.Fatalf("trial-v2 has %d live assignments, want 1", n)
	}

	// The superseded version must be active.
	again := createDraft(t, vr, "trial", true)
	if _, err := vr.Publish(ctx, again, "x", repository.SupersedeOptions{VersionID: "ttv-trial-v1", Force: true}, now); !errors.Is(err, repository.ErrSupersedeTargetNotActive) {
		t.Fatalf("supersede a retired version: err = %v, want ErrSupersedeTargetNotActive", err)
	}
}

func TestTierVersionPublishSupersedeGrandfathers(t *testing.T) {
	db := pgtest.MustDB()
	ctx := t.Context()
	vr := repository.NewTenantTierVersionRepository(db)
	ar := repository.NewTenantTierAssignmentRepository(db)
	now := time.Now().UTC()

	putOnVersion(t, ar, "ttv-trial-v1", now.Add(-time.Hour))
	next := createDraft(t, vr, "trial", true)

	reassigned, err := vr.Publish(ctx, next, "trial v2", repository.SupersedeOptions{VersionID: "ttv-trial-v1", Force: true}, now)
	if err != nil {
		t.Fatalf("publish superseding trial-v1: %v", err)
	}
	if reassigned != 0 {
		t.Fatalf("reassigned = %d, want 0 on force", reassigned)
	}
	if versionStatus(t, vr, "ttv-trial-v1") != models.TierVersionStatusRetired {
		t.Fatal("trial-v1 not retired")
	}
	if n, _ := vr.OpenAssignmentCount(ctx, "ttv-trial-v1"); n != 1 {
		t.Fatalf("force should keep the tenant on trial-v1, got %d live assignments", n)
	}
	cur, err := vr.ListCurrentPurchasable(ctx)
	if err != nil {
		t.Fatalf("list purchasable: %v", err)
	}
	if len(cur) != 1 || cur[0].ID != next {
		t.Fatalf("on sale = %+v, want only the new trial version", cur)
	}
}

// putOnVersion opens an assignment of the default tenant on a version.
func putOnVersion(t *testing.T, ar repository.TenantTierAssignmentRepository, versionID string, from time.Time) {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	a := &models.TenantTierAssignment{ID: id, TenantID: models.DefaultTenantID, TierVersionID: versionID, Reason: models.AssignmentReasonSignup, CreatedAt: from}
	if err := ar.Create(t.Context(), a, from, nil); err != nil {
		t.Fatalf("seed open assignment: %v", err)
	}
}
