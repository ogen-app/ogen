package repository_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// TestTenantTeardownFencePassesStatusUnderLock verifies the fence hands the
// callback the tenant's current lifecycle status (read under the row lock) — the
// signal the teardown worker branches on — for deleted, restored, and missing
// tenants.
func TestTenantTeardownFencePassesStatusUnderLock(t *testing.T) {
	db := openClassificationDB(t)
	ctx := t.Context()
	tenantRepo := repository.NewTenantRepository(db)
	fence := repository.NewTenantTeardownFence(db)

	id := mintID(t)
	seedTenant(t, db, id, "Acme", models.DefaultTierID)
	if err := tenantRepo.SoftDeleteTx(ctx, nil, id, time.Now().UTC()); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	assertStatus := func(want string) {
		t.Helper()
		var got string
		if err := fence.WithTenantLock(ctx, id, func(_ context.Context, status string) error {
			got = status
			return nil
		}); err != nil {
			t.Fatalf("WithTenantLock: %v", err)
		}
		if got != want {
			t.Fatalf("status under lock = %q, want %q", got, want)
		}
	}

	// Soft-deleted tenant.
	assertStatus(models.TenantStatusDeleted)

	// Restored tenant.
	if ok, err := tenantRepo.SetStatus(ctx, id, models.TenantStatusActive, "", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	assertStatus(models.TenantStatusActive)

	// Missing tenant → empty status, no error (nothing can restore it).
	var got = "sentinel"
	if err := fence.WithTenantLock(ctx, mintID(t), func(_ context.Context, status string) error {
		got = status
		return nil
	}); err != nil {
		t.Fatalf("WithTenantLock (missing): %v", err)
	}
	if got != "" {
		t.Fatalf("missing-tenant status = %q, want \"\"", got)
	}
}

// TestTenantTeardownFencePropagatesCallbackError verifies the callback's error
// surfaces so the teardown worker can retry.
func TestTenantTeardownFencePropagatesCallbackError(t *testing.T) {
	db := openClassificationDB(t)
	ctx := t.Context()
	fence := repository.NewTenantTeardownFence(db)

	id := mintID(t)
	seedTenant(t, db, id, "Acme", models.DefaultTierID)

	want := fmt.Errorf("boom")
	if err := fence.WithTenantLock(ctx, id, func(context.Context, string) error {
		return want
	}); err == nil {
		t.Fatalf("expected the callback error to propagate")
	}
}

// TestTenantTeardownFenceSerializesWithSetStatus is the core of CON-203's race
// fix: while a teardown holds the fence, a concurrent SetStatus(active) restore
// must block until the teardown finishes. This is what stops the worker from
// deleting the profile of a tenant that gets restored mid-teardown.
func TestTenantTeardownFenceSerializesWithSetStatus(t *testing.T) {
	db := openClassificationDB(t) // MaxOpenConns=2 → enough for both txns
	ctx := t.Context()
	tenantRepo := repository.NewTenantRepository(db)
	fence := repository.NewTenantTeardownFence(db)

	id := mintID(t)
	seedTenant(t, db, id, "Acme", models.DefaultTierID)
	if err := tenantRepo.SoftDeleteTx(ctx, nil, id, time.Now().UTC()); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- fence.WithTenantLock(ctx, id, func(_ context.Context, status string) error {
			if status != models.TenantStatusDeleted {
				return fmt.Errorf("unexpected status under lock: %q", status)
			}
			close(locked)
			<-release // hold the row lock until the test releases it
			return nil
		})
	}()
	<-locked

	// SetStatus(active) takes the same row lock, so it must not complete while the
	// teardown holds it.
	setDone := make(chan error, 1)
	go func() {
		_, err := tenantRepo.SetStatus(ctx, id, models.TenantStatusActive, "", time.Now().UTC())
		setDone <- err
	}()

	select {
	case <-setDone:
		t.Fatal("SetStatus(active) completed while the teardown held the tenant lock — not serialized")
	case <-time.After(200 * time.Millisecond):
		// Still blocked on the row lock — the fence is doing its job.
	}

	close(release)
	if err := <-fenceDone; err != nil {
		t.Fatalf("fence: %v", err)
	}

	select {
	case err := <-setDone:
		if err != nil {
			t.Fatalf("SetStatus after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetStatus did not complete after the teardown released the lock")
	}

	// The restore landed only after the teardown finished.
	st, err := tenantRepo.GetStatus(ctx, id)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st != models.TenantStatusActive {
		t.Fatalf("final status = %q, want active", st)
	}
}
