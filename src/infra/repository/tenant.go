package repository

import (
	"context"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// writeTenantID resolves the tenant_id to stamp on a RAW write. It mirrors the
// models.TenantScoped.BeforeAppendModel hook for the few repositories that build
// INSERTs with raw SQL (which bypasses bun model hooks). On a system context the
// caller-set value is trusted; otherwise the request tenant is required and a
// missing one fails closed.
func writeTenantID(ctx context.Context, current string) (string, error) {
	if tenantctx.IsSystem(ctx) {
		return current, nil
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return "", tenantctx.ErrNoTenant
	}
	return tid, nil
}

// scopeTenantRead reports the tenant a read must be filtered by. It mirrors the
// models.TenantScoped read hooks (BeforeSelect) for the repositories whose models
// are deliberately NOT TenantScoped — users and sessions, which the auth path
// looks up before a tenant is known. A tenant in context wins and must
// be applied (scoped=true); a system context reads across tenants (scoped=false);
// a missing tenant on a non-system context fails closed rather than leaking every
// tenant's rows.
func scopeTenantRead(ctx context.Context) (tenantID string, scoped bool, err error) {
	if tid, ok := tenantctx.From(ctx); ok {
		return tid, true, nil
	}
	if tenantctx.IsSystem(ctx) {
		return "", false, nil
	}
	return "", false, tenantctx.ErrNoTenant
}
