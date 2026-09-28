package models

import (
	"context"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TenantScoped is embedded (anonymously) by every tenant-owned model to enforce
// tenant isolation centrally. bun fires these query hooks on the
// model, so the tenant predicate is added in ONE place — no repository method
// can forget to scope, which is the whole point of app-level enforcement:
//
//   - BeforeSelect / BeforeUpdate / BeforeDelete add `tenant_id = ?` from the
//     context's tenant, so reads/updates/deletes never cross a tenant boundary.
//   - BeforeAppendModel stamps tenant_id from the context on INSERT, so writes
//     are never trusted to carry their own tenant.
//
// Missing tenant context fails closed (ErrNoTenant) — a query refuses to run
// rather than touch every tenant's rows — UNLESS the context is a system
// context (tenantctx.WithSystem), used for backfills, seeds, and background
// jobs that legitimately span tenants and set tenant_id explicitly.
//
// Note: users and sessions are NOT TenantScoped. The auth path looks them up
// by email/token BEFORE a tenant is known (that lookup is how the tenant is
// discovered), so auto-scoping them would deadlock the login flow. They carry a
// plain tenant_id column instead.
type TenantScoped struct {
	TenantID string `bun:"tenant_id,notnull" json:"-"`
}

var (
	_ schema.BeforeAppendModelHook = (*TenantScoped)(nil)
	_ bun.BeforeSelectHook         = (*TenantScoped)(nil)
	_ bun.BeforeUpdateHook         = (*TenantScoped)(nil)
	_ bun.BeforeDeleteHook         = (*TenantScoped)(nil)
)

// BeforeAppendModel stamps tenant_id from the request context on INSERT. On a
// system context it trusts the caller-set TenantID. UPDATE value-building is
// left untouched so an update can never move a row to another tenant.
func (m *TenantScoped) BeforeAppendModel(ctx context.Context, query schema.Query) error {
	if _, ok := query.(*bun.InsertQuery); !ok {
		return nil
	}
	// An explicit tenant always wins — even inside a system context — so a job
	// that has resolved its entity's tenant (tenantctx.With over its system
	// ctx) writes into the right tenant.
	if tid, ok := tenantctx.From(ctx); ok {
		m.TenantID = tid
		return nil
	}
	if tenantctx.IsSystem(ctx) {
		// A system writer with no tenant in context (e.g. the Zernio worker
		// writing instance settings) lands in the default tenant rather than
		// violating NOT NULL.
		if m.TenantID == "" {
			m.TenantID = DefaultTenantID
		}
		return nil
	}
	return tenantctx.ErrNoTenant
}

func (m *TenantScoped) BeforeSelect(ctx context.Context, q *bun.SelectQuery) error {
	return scopeQuery(ctx, func(tid string) { q.Where("?TableAlias.tenant_id = ?", tid) })
}

func (m *TenantScoped) BeforeUpdate(ctx context.Context, q *bun.UpdateQuery) error {
	return scopeQuery(ctx, func(tid string) { q.Where("?TableAlias.tenant_id = ?", tid) })
}

func (m *TenantScoped) BeforeDelete(ctx context.Context, q *bun.DeleteQuery) error {
	return scopeQuery(ctx, func(tid string) { q.Where("?TableAlias.tenant_id = ?", tid) })
}

// scopeQuery applies the tenant predicate, or fails closed when no tenant is in
// context. System contexts skip filtering entirely.
func scopeQuery(ctx context.Context, apply func(tenantID string)) error {
	// Tenant wins over system: once a tenant is known, scope to it even inside
	// a system context.
	if tid, ok := tenantctx.From(ctx); ok {
		apply(tid)
		return nil
	}
	if tenantctx.IsSystem(ctx) {
		return nil
	}
	return tenantctx.ErrNoTenant
}
