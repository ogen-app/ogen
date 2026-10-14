package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TenantTeardownFence serializes a tenant's Zernio profile teardown
// against a concurrent lifecycle change (CON-190 restore). It is a deliberately
// tiny type — not part of TenantRepository — so adding it doesn't ripple through
// that interface's many fakes.
//
// The fence works purely through the tenant row lock: SetStatus (the restore
// path) already does its read-modify-write under `SELECT ... FOR UPDATE` on the
// tenant row, so a teardown that holds that same lock across its destructive
// Zernio calls cannot be overtaken by a restore. Either the teardown observes a
// still-deleted tenant and proceeds (restore then blocks until it finishes), or
// the restore committed first and the teardown observes `active` and skips.
type TenantTeardownFence struct{ db *bun.DB }

// NewTenantTeardownFence builds the fence around the shared DB handle.
func NewTenantTeardownFence(db *bun.DB) *TenantTeardownFence {
	return &TenantTeardownFence{db: db}
}

// WithTenantLock runs fn while holding `SELECT ... FOR UPDATE` on the tenant
// row, passing fn the tenant's current lifecycle status read under that lock. A
// concurrent SetStatus (which takes the same lock) cannot commit until fn
// returns, so no restore can slip in mid-teardown.
//
// A missing row (a hard-deleted tenant) runs fn with status "" and no lock —
// there is nothing that can restore it, so there is nothing to race. fn's error
// is returned as-is (the surrounding tx holds no writes of its own, so a
// rollback only releases the lock). fn does external I/O, so the lock — and one
// pooled connection — is held for the teardown's duration; teardown jobs are
// rare (one per workspace delete) and bounded by the worker's timeout.
func (f *TenantTeardownFence) WithTenantLock(ctx context.Context, tenantID string, fn func(ctx context.Context, status string) error) error {
	return f.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var status string
		err := tx.NewSelect().Model((*models.Tenant)(nil)).
			Column("status").
			Where("id = ?", tenantID).
			For("UPDATE").
			Scan(ctx, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return fn(ctx, "")
		}
		if err != nil {
			return err
		}
		return fn(ctx, status)
	})
}

// ErrZernioTeardownInProgress means a restore was refused because river-service
// is still tearing down the tenant's Zernio profile. It clears within one
// lease.
var ErrZernioTeardownInProgress = errors.New("zernio profile teardown in progress; retry shortly")

// BeginTeardownLease starts a remote Zernio teardown: under the tenant row
// lock it reads the status and, unless the tenant is active again, takes a
// lease that holds off a restore until it ends. found is false for a
// hard-deleted tenant, which nothing can restore, so no lease is needed.
func (f *TenantTeardownFence) BeginTeardownLease(ctx context.Context, tenantID string, ttl time.Duration) (status string, found bool, err error) {
	err = f.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		err := tx.NewSelect().Model((*models.Tenant)(nil)).
			Column("status").
			Where("id = ?", tenantID).
			For("UPDATE").
			Scan(ctx, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if status == models.TenantStatusActive {
			return nil
		}
		_, err = tx.NewUpdate().Model((*models.Tenant)(nil)).
			Set("zernio_teardown_lease_until = ?", time.Now().UTC().Add(ttl)).
			Where("id = ?", tenantID).
			Exec(ctx)
		return err
	})
	return status, found, err
}

// RenewTeardownLease extends the lease before the next destructive Zernio
// call. It reports false when the tenant is active again (a restore landed
// after the lease lapsed), so the teardown must stop. A hard-deleted tenant
// reports true.
func (f *TenantTeardownFence) RenewTeardownLease(ctx context.Context, tenantID string, ttl time.Duration) (bool, error) {
	res, err := f.db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("zernio_teardown_lease_until = ?", time.Now().UTC().Add(ttl)).
		Where("id = ? AND status <> ?", tenantID, models.TenantStatusActive).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	exists, err := f.db.NewSelect().Model((*models.Tenant)(nil)).Where("id = ?", tenantID).Exists(ctx)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// EndTeardownLease releases the lease once the teardown has finished or
// given up.
func (f *TenantTeardownFence) EndTeardownLease(ctx context.Context, tenantID string) error {
	_, err := f.db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("zernio_teardown_lease_until = NULL").
		Where("id = ?", tenantID).
		Exec(ctx)
	return err
}
