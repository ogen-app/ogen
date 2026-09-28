package queues

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TenantStatusReader reads a tenant's lifecycle status so per-tenant
// jobs can skip work for suspended or deleted tenants. repository.TenantRepository
// satisfies it via GetStatus; a narrow interface keeps the workers test-friendly.
type TenantStatusReader interface {
	GetStatus(ctx context.Context, id string) (string, error)
}

// tenantIsActive reports whether the tenant may have background work run for it.
// It is the job-side companion to the auth chokepoint: the auth path
// already blocks a suspended/deleted tenant's users, and this stops their
// already-enqueued automated work (publish, bootstrap, email) from running.
//
// A nil reader (not wired — e.g. in tests) or an empty tenant id is treated as
// active so the guard never silently drops work when unconfigured; the auth
// chokepoint remains the definitive enforcement. An unknown tenant (ErrNoRows)
// is "not active" — there is nothing to act on. Any other error is returned so
// the caller can retry rather than skip on a transient DB failure.
func tenantIsActive(ctx context.Context, reader TenantStatusReader, tenantID string) (bool, error) {
	if reader == nil || tenantID == "" {
		return true, nil
	}
	st, err := reader.GetStatus(ctx, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st == models.TenantStatusActive, nil
}
